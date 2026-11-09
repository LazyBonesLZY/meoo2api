// Package account 管理 Meoo 账号：登录态、个人 AK、项目 AK 开通与入池。
package account

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

var AllScopes = []string{
	"cli.compat", "project.read", "project.write", "cloud.read", "cloud.write",
	"cloud.credentials.read", "cloud.database.read", "cloud.function.read",
	"credit.read", "entitlement.read", "user.read", "release.read", "source.read",
}

type Service struct {
	St *store.Store
	Up *upstream.Client
}

func (s *Service) Find(id string) *model.Account {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	for _, a := range s.St.Data.Accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// Identify 用登录态确认账号身份
func (s *Service) Identify(a *model.Account) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", "https://meoo.com/api/v1/user/me", nil,
		map[string]string{"Cookie": a.Cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return err
	}
	var d struct {
		Data struct {
			UserID   string `json:"user_id"`
			Username string `json:"username"`
			Phone    string `json:"phone"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 || d.Data.UserID == "" {
		return fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Username
	if p := d.Data.Phone; len(p) > 7 {
		name = fmt.Sprintf("%s (%s****%s)", name, p[:3], p[len(p)-4:])
	}
	s.St.Mu.Lock()
	a.UserID = d.Data.UserID
	a.Name = name
	if a.Label == "" || a.Label == "未命名" {
		a.Label = name
	}
	s.St.Mu.Unlock()
	s.St.Touch()
	return nil
}

// IdentifyKey 用项目 AK 查账号（多数项目 AK 查不到，返回提示）
func (s *Service) IdentifyKey(k *model.Key) (*model.Identity, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", "https://meoo.com/open/v1/cli-compat/account/identity", nil,
		map[string]string{"Authorization": "Bearer " + k.Key, "X-Meoo-Client": "cli"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Data struct {
			UserID   string `json:"user_id"`
			Name     string `json:"name"`
			Username string `json:"username"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 || d.Data.UserID == "" {
		if code == 401 {
			return nil, fmt.Errorf("项目服务 AK 查不到账号（该接口只认个人 AK）；由账号一键接入时会自动绑定")
		}
		return nil, fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Name
	if name == "" {
		name = d.Data.Username
	}
	id := &model.Identity{ID: d.Data.UserID, Name: name, Kind: "key"}
	s.St.Mu.Lock()
	k.Account = id
	s.St.Mu.Unlock()
	s.St.Touch()
	return id, nil
}

// Ping 轻量请求，兼作会话保活
func (s *Service) Ping(a *model.Account) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", "https://meoo.com/api/v1/user/me", nil,
		map[string]string{"Cookie": a.Cookie, "Referer": "https://meoo.com/"})
	ok := false
	if err == nil && code == 200 {
		var d struct {
			Data struct {
				UserID string `json:"user_id"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &d)
		ok = d.Data.UserID != ""
	}
	s.St.Mu.Lock()
	a.LastPing = time.Now().UnixMilli()
	a.PingOK = ok
	a.Expired = !ok
	if ok && a.UserID == "" {
		a.Expired = false
	}
	s.St.Mu.Unlock()
	s.St.Touch()
	if !ok {
		if err != nil {
			return false, err
		}
		return false, fmt.Errorf("会话已失效 HTTP %d", code)
	}
	return true, nil
}

// PersonalKey 在该账号下创建个人 AK（不可进模型池）
func (s *Service) PersonalKey(a *model.Account) (*model.PersonalKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "POST", "https://meoo.com/api/v1/user/api-keys", map[string]any{
		"name": "meoo2api-console", "description": "created by meoo2api console", "scopes": AllScopes,
	}, map[string]string{"Cookie": a.Cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Data struct {
			APIKey string   `json:"apiKey"`
			Scopes []string `json:"scopes"`
		} `json:"data"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 || d.Data.APIKey == "" {
		msg := d.Error.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		return nil, fmt.Errorf("创建失败 HTTP %d %s", code, msg)
	}
	pk := &model.PersonalKey{ID: model.RandID("pk_", 6), AccountID: a.ID, Name: "meoo2api-console",
		Key: d.Data.APIKey, Scopes: d.Data.Scopes, CreatedAt: time.Now().UnixMilli()}
	s.St.Mu.Lock()
	s.St.Data.PersonalKeys = append(s.St.Data.PersonalKeys, pk)
	a.PersonalKeyIDs = append(a.PersonalKeyIDs, pk.ID)
	s.St.Mu.Unlock()
	s.St.Touch()
	_ = s.St.Save()
	return pk, nil
}

// ProjectKey 用账号的个人 AK 建项目 → 开云服务 → 取项目 AK → 入号池
func (s *Service) ProjectKey(a *model.Account, projectName string) (map[string]any, error) {
	personalAK := s.personalAKOf(a)
	if personalAK == "" {
		return nil, fmt.Errorf("该账号还没有个人 AK，先创建再接入号池")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + personalAK, "X-Meoo-Client": "cli"}
	code, raw, err := s.Up.DoJSON(ctx, "POST", "https://meoo.com/open/v1/cli-compat/projects",
		map[string]any{"name": projectName, "type": "create-desktop"}, hdr)
	if err != nil {
		return nil, err
	}
	var proj struct {
		Data struct {
			URLID string `json:"url_id"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &proj)
	if code != 200 || proj.Data.URLID == "" {
		return nil, fmt.Errorf("建项目失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	urlID := proj.Data.URLID
	code, raw, err = s.Up.DoJSON(ctx, "POST",
		fmt.Sprintf("https://meoo.com/open/v1/cli-compat/projects/%s/cloud", urlID),
		map[string]any{"description": projectName + " cloud"}, hdr)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return map[string]any{"project": urlID, "stage": "cloud",
			"error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
	}
	code, raw, err = s.Up.DoJSON(ctx, "GET",
		fmt.Sprintf("https://meoo.com/open/v1/cli-compat/projects/%s/cloud/environment", urlID), nil, hdr)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			Key string `json:"meooProjectApiKey"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if code != 200 || env.Data.Key == "" {
		return map[string]any{"project": urlID, "stage": "environment",
			"error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
	}
	k := store.NewKey(a.Label+" / "+urlID, env.Data.Key, 1)
	k.Account = &model.Identity{ID: a.UserID, Name: a.Name, Kind: "key"}
	k.AccountID = a.ID
	s.St.Mu.Lock()
	s.St.Data.Keys = append(s.St.Data.Keys, k)
	a.ProjectKeyIDs = append(a.ProjectKeyIDs, k.ID)
	s.St.Mu.Unlock()
	s.St.Touch()
	_ = s.St.Save()
	return map[string]any{"ok": true, "project": urlID, "keyId": k.ID, "key": model.Mask(k.Key)}, nil
}

// Onboard 一键接入：识别 → 个人 AK → 项目 AK → 入池
func (s *Service) Onboard(a *model.Account) (map[string]any, error) {
	out := map[string]any{}
	if a.Name == "" || a.UserID == "" {
		if err := s.Identify(a); err != nil {
			return nil, err
		}
	}
	out["account"] = map[string]any{"userId": a.UserID, "name": a.Name}
	if s.personalAKOf(a) == "" {
		pk, err := s.PersonalKey(a)
		if err != nil {
			return out, err
		}
		out["personalKey"] = model.Mask(pk.Key)
	}
	proj, err := s.ProjectKey(a, "meoo2api-"+strings.ToLower(model.RandID("", 3)))
	if err != nil {
		return out, err
	}
	for k, v := range proj {
		out[k] = v
	}
	return out, nil
}

func (s *Service) personalAKOf(a *model.Account) string {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	for _, pk := range s.St.Data.PersonalKeys {
		if pk.AccountID == a.ID || (a.UserID != "" && pk.AccountID == a.UserID) {
			return pk.Key
		}
	}
	return ""
}

// PersonalKeyOf 供控制台展示
func (s *Service) PersonalKeyOf(a *model.Account) []*model.PersonalKey {
	s.St.Mu.RLock()
	defer s.St.Mu.RUnlock()
	out := []*model.PersonalKey{}
	for _, pk := range s.St.Data.PersonalKeys {
		if pk.AccountID == a.ID || (a.UserID != "" && pk.AccountID == a.UserID) {
			out = append(out, pk)
		}
	}
	return out
}
