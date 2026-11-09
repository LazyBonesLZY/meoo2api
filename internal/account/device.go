package account

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"meoo2api/internal/model"
)

const (
	deviceCreate = "https://meoo.com/api/v1/cli/auth/session"
	devicePoll   = "https://meoo.com/api/v1/cli/auth/session/%s"
	loginPage    = "https://meoo.com/cli/login?session_id=%s"
)

// StartDevice 创建 CLI 设备授权会话（无需凭证）
func (s *Service) StartDevice() (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "POST", deviceCreate, map[string]any{}, nil)
	if err != nil {
		return "", "", err
	}
	var d struct {
		Success bool `json:"success"`
		Data    struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 || d.Data.SessionID == "" {
		return "", "", fmt.Errorf("创建授权会话失败 HTTP %d", code)
	}
	return d.Data.SessionID, fmt.Sprintf(loginPage, d.Data.SessionID), nil
}

type DeviceState struct {
	Status   string `json:"status"`
	APIKey   string `json:"apiKey"`
	UserID   string `json:"userId"`
	Username string `json:"username"`
}

// PollDevice 查询授权状态：pending / approved / expired
func (s *Service) PollDevice(sid string) (*DeviceState, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", fmt.Sprintf(devicePoll, sid), nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Success bool        `json:"success"`
		Data    DeviceState `json:"data"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 {
		return nil, fmt.Errorf("查询授权状态失败 HTTP %d", code)
	}
	return &d.Data, nil
}

// AddFromDevice 用已拿到的授权结果入库并接入号池。
// 注意：session 只能读取一次，approved 之后再次轮询会变成 expired，
// 所以授权结果必须由调用方传入，不能在这里重新轮询。
func (s *Service) AddFromDevice(st *DeviceState) (*model.Account, map[string]any, error) {
	if st == nil || st.Status != "approved" || st.APIKey == "" {
		if st == nil {
			return nil, nil, fmt.Errorf("授权结果为空")
		}
		return nil, map[string]any{"status": st.Status}, fmt.Errorf("尚未授权（%s）", st.Status)
	}
	s.St.Mu.Lock()
	var a *model.Account
	for _, x := range s.St.Data.Accounts {
		if x.UserID != "" && x.UserID == st.UserID {
			a = x
			break
		}
	}
	if a == nil {
		a = &model.Account{ID: model.RandID("a_", 6), UserID: st.UserID, Name: st.Username,
			Enabled: true, CreatedAt: time.Now().UnixMilli(), PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{}}
		s.St.Data.Accounts = append(s.St.Data.Accounts, a)
	}
	pk := &model.PersonalKey{ID: model.RandID("pk_", 6), AccountID: st.UserID, Name: "cli-device",
		Key: st.APIKey, CreatedAt: time.Now().UnixMilli()}
	s.St.Data.PersonalKeys = append(s.St.Data.PersonalKeys, pk)
	a.PersonalKeyIDs = append(a.PersonalKeyIDs, pk.ID)
	s.St.Mu.Unlock()
	s.St.Touch()

	out := map[string]any{"userId": st.UserID, "name": st.Username}
	if v, err := s.Points(a); err == nil {
		out["points"] = v
	}
	if a.KeyID == "" {
		proj, _ := s.ProjectKey(a, "meoo2api-"+strings.ToLower(model.RandID("", 3)))
		for k, v := range proj {
			out[k] = v
		}
	} else {
		out["key"] = s.keyInfo(a)
	}
	_ = s.St.Save()
	return a, out, nil
}
