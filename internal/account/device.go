package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"meoo2api/internal/model"
)

const (
	deviceCreate      = "https://meoo.com/api/v1/cli/auth/session"
	devicePoll        = "https://meoo.com/api/v1/cli/auth/session/%s"
	loginPage         = "https://meoo.com/cli/login?session_id=%s"
	deviceTTL         = 15 * time.Minute
	maxDeviceSessions = 256
)

type deviceSession struct {
	gate      chan struct{}
	expires   time.Time
	state     *DeviceState
	accountID string
	result    map[string]any
	nextPoll  time.Time
}

// APIKey is an internal one-shot credential, never a browser response field.
type DeviceState struct {
	Status    string `json:"status"`
	APIKey    string `json:"-"`
	UserID    string `json:"userId"`
	Username  string `json:"username"`
	sessionID string
}

func (s *Service) deviceLocked(sid string) *deviceSession {
	now := time.Now()
	for id, d := range s.devices {
		if now.Before(d.expires) {
			continue
		}
		select {
		case d.gate <- struct{}{}:
			delete(s.devices, id)
			if d.state != nil {
				d.state.APIKey = ""
			}
			<-d.gate
		default:
		}
	}
	return s.devices[sid]
}
func deviceAcquire(ctx context.Context, d *deviceSession) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case d.gate <- struct{}{}:
		return func() { <-d.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (s *Service) StartDevice() (string, string, error) {
	return s.StartDeviceContext(context.Background())
}
func (s *Service) StartDeviceContext(ctx context.Context) (string, string, error) {
	s.deviceMu.Lock()
	s.deviceLocked("")
	full := len(s.devices) >= maxDeviceSessions
	s.deviceMu.Unlock()
	if full {
		return "", "", fmt.Errorf("设备授权会话数量已达上限")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodPost, deviceCreate, map[string]any{}, nil)
	if err != nil {
		return "", "", err
	}
	var d struct {
		Success *bool `json:"success"`
		Data    struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &d) != nil || d.Data.SessionID == "" || (d.Success != nil && !*d.Success) {
		return "", "", fmt.Errorf("创建授权会话失败 HTTP %d", code)
	}
	s.deviceMu.Lock()
	defer s.deviceMu.Unlock()
	if s.devices == nil {
		s.devices = map[string]*deviceSession{}
	}
	s.deviceLocked("")
	if len(s.devices) >= maxDeviceSessions {
		return "", "", fmt.Errorf("设备授权会话数量已达上限")
	}
	if _, exists := s.devices[d.Data.SessionID]; exists {
		return "", "", fmt.Errorf("上游重复返回设备授权会话")
	}
	s.devices[d.Data.SessionID] = &deviceSession{gate: make(chan struct{}, 1), expires: time.Now().Add(deviceTTL)}
	return d.Data.SessionID, fmt.Sprintf(loginPage, url.QueryEscape(d.Data.SessionID)), nil
}

// PollDevice serializes upstream consumption and caches approved/terminal states.
// Retrying a poll after approval never consumes the upstream session twice.
func (s *Service) PollDevice(sid string) (*DeviceState, error) {
	return s.PollDeviceContext(context.Background(), sid)
}
func (s *Service) PollDeviceContext(ctx context.Context, sid string) (*DeviceState, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" || len(sid) > 1024 {
		return nil, fmt.Errorf("设备授权会话无效")
	}
	s.deviceMu.Lock()
	d := s.deviceLocked(sid)
	s.deviceMu.Unlock()
	if d == nil {
		return nil, fmt.Errorf("设备授权会话不存在或已过期")
	}
	release, err := deviceAcquire(ctx, d)
	if err != nil {
		return nil, err
	}
	defer release()
	if !time.Now().Before(d.expires) {
		return &DeviceState{Status: "expired", sessionID: sid}, nil
	}
	if d.state != nil && (d.state.Status != "pending" || time.Now().Before(d.nextPoll)) {
		cp := *d.state
		return &cp, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	code, raw, err := s.request(ctx, http.MethodGet, fmt.Sprintf(devicePoll, url.PathEscape(sid)), nil, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Success *bool `json:"success"`
		Data    struct {
			Status   string `json:"status"`
			APIKey   string `json:"apiKey"`
			UserID   string `json:"userId"`
			Username string `json:"username"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &response) != nil || (response.Success != nil && !*response.Success) {
		return nil, fmt.Errorf("查询授权状态失败 HTTP %d", code)
	}
	v := response.Data
	switch v.Status {
	case "pending", "expired", "denied":
	case "approved":
		if v.APIKey == "" {
			return nil, fmt.Errorf("授权响应缺少个人凭证")
		}
	default:
		return nil, fmt.Errorf("授权响应状态无效")
	}
	if v.Status != "approved" {
		v.APIKey = ""
	}
	d.state = &DeviceState{Status: v.Status, APIKey: v.APIKey, UserID: v.UserID, Username: v.Username, sessionID: sid}
	d.nextPoll = time.Now().Add(time.Second)
	cp := *d.state
	return &cp, nil
}
func (s *Service) AddFromDevice(st *DeviceState) (*model.Account, map[string]any, error) {
	return s.AddFromDeviceContext(context.Background(), st)
}
func copyResult(v map[string]any) map[string]any {
	cp := map[string]any{}
	for k, x := range v {
		cp[k] = x
	}
	return cp
}

// AddFromDevice accepts the already consumed state; never re-polls the device.
func (s *Service) AddFromDeviceContext(ctx context.Context, st *DeviceState) (*model.Account, map[string]any, error) {
	if st == nil {
		return nil, nil, fmt.Errorf("授权结果为空")
	}
	var session *deviceSession
	if st.sessionID != "" {
		s.deviceMu.Lock()
		session = s.deviceLocked(st.sessionID)
		s.deviceMu.Unlock()
		if session == nil {
			return nil, nil, fmt.Errorf("设备授权会话已过期")
		}
		release, err := deviceAcquire(ctx, session)
		if err != nil {
			return nil, nil, err
		}
		defer release()
		if !time.Now().Before(session.expires) {
			return nil, nil, fmt.Errorf("设备授权会话已过期")
		}
		if session.accountID != "" {
			a := s.Find(session.accountID)
			if a == nil {
				return nil, nil, fmt.Errorf("已接入账号已删除")
			}
			if session.result != nil {
				return a, copyResult(session.result), nil
			}
		}
		// Read the private cache rather than trusting caller mutations to the state.
		if session.state == nil {
			return nil, nil, fmt.Errorf("设备授权状态尚未读取")
		}
		cp := *session.state
		st = &cp
	}
	if st.Status != "approved" || st.APIKey == "" {
		return nil, map[string]any{"status": st.Status}, fmt.Errorf("尚未取得有效授权结果")
	}
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if session != nil && session.accountID != "" && s.Find(session.accountID) == nil {
		return nil, nil, fmt.Errorf("已接入账号已删除")
	}
	// Bind the personal AK to an independently verified identity before persistence.
	v, err := s.identity(ctx, "", st.APIKey)
	if err != nil {
		return nil, nil, err
	}
	if st.UserID != "" && st.UserID != v.UserID {
		return nil, nil, fmt.Errorf("设备授权凭证与身份不一致")
	}
	s.St.Mu.Lock()
	var a *model.Account
	for _, x := range s.St.Data.Accounts {
		if x.UserID == v.UserID {
			a = x
			break
		}
	}
	if a == nil {
		a = &model.Account{ID: model.RandID("a_", 6), UserID: v.UserID, Enabled: true, CreatedAt: time.Now().UnixMilli(), PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{}}
		s.St.Data.Accounts = append(s.St.Data.Accounts, a)
	}
	a.Name = v.Name
	if a.Label == "" || a.Label == "未命名" {
		a.Label = v.Name
	}
	s.upsertPersonalLocked(a, st.APIKey, "cli-device", nil)
	// A valid personal AK does not rehabilitate an expired browser Cookie.
	if a.Cookie == "" {
		a.Expired = false
		a.PingOK = true
		a.LastPing = time.Now().UnixMilli()
	}
	s.St.Mu.Unlock()
	out := map[string]any{"userId": v.UserID, "name": v.Name}
	if session != nil {
		session.accountID = a.ID
	}
	if err := s.persist(); err != nil {
		return a, out, err
	}
	proj, err := s.projectKey(ctx, a, "")
	for k, v := range proj {
		out[k] = v
	}
	if ctx.Err() == nil {
		if points, pointsErr := s.PointsContext(ctx, a); pointsErr == nil {
			out["points"] = points
		}
	}
	if saveErr := s.persist(); err == nil {
		err = saveErr
	}
	if err == nil && session != nil {
		session.accountID = a.ID
		session.result = copyResult(out)
		session.state.APIKey = ""
	}
	return a, out, err
}
