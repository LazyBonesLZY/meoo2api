// Package account 处理账号身份识别与自动开通（个人 AK / 项目服务 AK）。
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

func (s *Service) IdentifyKey(k *model.Key) (*model.Account, error) {
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
			return nil, fmt.Errorf("项目服务 AK 查不到账号（该接口只认个人 AK）；用「登录态开项目」创建时会自动绑定")
		}
		return nil, fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Name
	if name == "" {
		name = d.Data.Username
	}
	acc := &model.Account{ID: d.Data.UserID, Name: name, Kind: "key"}
	s.St.Mu.Lock()
	k.Account = acc
	s.St.Mu.Unlock()
	s.St.Touch()
	return acc, nil
}

func (s *Service) IdentifyCheckin(c *model.Checkin) (*model.Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", "https://meoo.com/api/v1/user/me", nil,
		map[string]string{"Cookie": c.Cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Username
	if p := d.Data.Phone; len(p) > 7 {
		name = fmt.Sprintf("%s (%s****%s)", name, p[:3], p[len(p)-4:])
	}
	acc := &model.Account{ID: d.Data.UserID, Name: name, Kind: "session"}
	s.St.Mu.Lock()
	c.Account = acc
	s.St.Mu.Unlock()
	s.St.Touch()
	return acc, nil
}

// Ping 用登录态打一次轻量接口，兼作会话保活
func (s *Service) Ping(c *model.Checkin) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "GET", "https://meoo.com/api/v1/user/me", nil,
		map[string]string{"Cookie": c.Cookie, "Referer": "https://meoo.com/"})
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
	c.LastPing = time.Now().UnixMilli()
	c.PingOK = ok
	c.Expired = !ok
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

func (s *Service) CreatePersonalKey(c *model.Checkin) (*model.PersonalKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	code, raw, err := s.Up.DoJSON(ctx, "POST", "https://meoo.com/api/v1/user/api-keys", map[string]any{
		"name": "meoo2api-console", "description": "created by meoo2api console", "scopes": AllScopes,
	}, map[string]string{"Cookie": c.Cookie, "Referer": "https://meoo.com/"})
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
	pk := &model.PersonalKey{ID: model.RandID("pk_", 6), Name: "meoo2api-console",
		Key: d.Data.APIKey, Scopes: d.Data.Scopes, CreatedAt: time.Now().UnixMilli()}
	if c.Account != nil {
		pk.AccountID = c.Account.ID
	}
	s.St.Mu.Lock()
	s.St.Data.PersonalKeys = append(s.St.Data.PersonalKeys, pk)
	s.St.Mu.Unlock()
	s.St.Touch()
	_ = s.St.Save()
	return pk, nil
}

// ProvisionProject 建项目 → 开云服务 → 取项目服务 AK 入池
func (s *Service) ProvisionProject(personalAK, name string, acc *model.Account) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + personalAK, "X-Meoo-Client": "cli"}
	code, raw, err := s.Up.DoJSON(ctx, "POST", "https://meoo.com/open/v1/cli-compat/projects",
		map[string]any{"name": name, "type": "create-desktop"}, hdr)
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
		map[string]any{"description": name + " cloud"}, hdr)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return map[string]any{"project": urlID, "stage": "cloud", "error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
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
		return map[string]any{"project": urlID, "stage": "environment", "error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
	}
	k := store.NewKey("账号 "+urlID, env.Data.Key, 1)
	if acc != nil {
		k.Account = &model.Account{ID: acc.ID, Name: acc.Name, Kind: "key"}
	}
	s.St.Mu.Lock()
	s.St.Data.Keys = append(s.St.Data.Keys, k)
	s.St.Mu.Unlock()
	s.St.Touch()
	_ = s.St.Save()
	return map[string]any{"ok": true, "project": urlID, "keyId": k.ID, "key": model.Mask(k.Key)}, nil
}
