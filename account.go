package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind,omitempty"` // key | session
}

type PersonalKey struct {
	ID        string   `json:"id"`
	AccountID string   `json:"accountId"`
	Name      string   `json:"name"`
	Key       string   `json:"key"`
	Scopes    []string `json:"scopes"`
	CreatedAt int64    `json:"createdAt"`
}

var AllScopes = []string{
	"cli.compat", "project.read", "project.write", "cloud.read", "cloud.write",
	"cloud.credentials.read", "cloud.database.read", "cloud.function.read",
	"credit.read", "entitlement.read", "user.read", "release.read", "source.read",
}

func doJSON(ctx context.Context, method, url string, body any, hdr map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// IdentifyKey 用项目 AK 查账号
func (s *State) IdentifyKey(k *Key) (*Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, raw, err := doJSON(ctx, http.MethodGet, "https://meoo.com/open/v1/cli-compat/account/identity", nil,
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
			return nil, fmt.Errorf("项目服务 AK 查不到账号（该接口只认个人 AK）。用「登录态开项目」创建时会自动绑定账号")
		}
		return nil, fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Name
	if name == "" {
		name = d.Data.Username
	}
	acc := &Account{ID: d.Data.UserID, Name: name, Kind: "key"}
	s.mu.Lock()
	k.Account = acc
	s.mu.Unlock()
	s.Touch()
	return acc, nil
}

// IdentifyCheckin 用网页登录态查账号
func (s *State) IdentifyCheckin(c *Checkin) (*Account, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	code, raw, err := doJSON(ctx, http.MethodGet, "https://meoo.com/api/v1/user/me", nil,
		map[string]string{"Cookie": c.Cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Success bool `json:"success"`
		Data    struct {
			UserID   string `json:"user_id"`
			Username string `json:"username"`
			Phone    string `json:"phone"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &d)
	if code != 200 || d.Data.UserID == "" {
		if code == 401 {
			return nil, fmt.Errorf("项目服务 AK 查不到账号（该接口只认个人 AK）。用「登录态开项目」创建时会自动绑定账号")
		}
		return nil, fmt.Errorf("识别失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	name := d.Data.Username
	if d.Data.Phone != "" {
		p := d.Data.Phone
		if len(p) > 7 {
			p = p[:3] + "****" + p[len(p)-4:]
		}
		name = fmt.Sprintf("%s (%s)", name, p)
	}
	acc := &Account{ID: d.Data.UserID, Name: name, Kind: "session"}
	s.mu.Lock()
	c.Account = acc
	s.mu.Unlock()
	s.Touch()
	return acc, nil
}

// CreatePersonalKey 用登录态在该账号下创建 API Key（meoo_ak_，不能被模型网关接受）
func (s *State) CreatePersonalKey(c *Checkin) (*PersonalKey, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body := map[string]any{
		"name":        "meoo2api-console",
		"description": "created by meoo2api console",
		"scopes":      AllScopes,
	}
	code, raw, err := doJSON(ctx, http.MethodPost, "https://meoo.com/api/v1/user/api-keys", body,
		map[string]string{"Cookie": c.Cookie, "Referer": "https://meoo.com/"})
	if err != nil {
		return nil, err
	}
	var d struct {
		Success bool `json:"success"`
		Data    struct {
			ID     string   `json:"id"`
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
	accID := ""
	if c.Account != nil {
		accID = c.Account.ID
	}
	pk := &PersonalKey{
		ID: randID("pk_", 6), AccountID: accID, Name: "meoo2api-console",
		Key: d.Data.APIKey, Scopes: d.Data.Scopes, CreatedAt: time.Now().UnixMilli(),
	}
	s.mu.Lock()
	s.PersonalKeys = append(s.PersonalKeys, pk)
	s.mu.Unlock()
	s.Touch()
	_ = s.Save()
	return pk, nil
}

// ProvisionProject 用登录态对应账号的个人 AK 建项目 → 开云服务 → 取项目服务 AK 入池
func (s *State) ProvisionProject(personalAK, projectName string, acc *Account) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	hdr := map[string]string{"Authorization": "Bearer " + personalAK, "X-Meoo-Client": "cli"}
	code, raw, err := doJSON(ctx, http.MethodPost, "https://meoo.com/open/v1/cli-compat/projects",
		map[string]any{"name": projectName, "type": "create-desktop"}, hdr)
	if err != nil {
		return nil, err
	}
	var proj struct {
		Data struct {
			URLID string `json:"url_id"`
			Name  string `json:"name"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &proj)
	if code != 200 || proj.Data.URLID == "" {
		return nil, fmt.Errorf("建项目失败 HTTP %d %s", code, strings.TrimSpace(string(raw)))
	}
	urlID := proj.Data.URLID

	code, raw, err = doJSON(ctx, http.MethodPost,
		fmt.Sprintf("https://meoo.com/open/v1/cli-compat/projects/%s/cloud", urlID),
		map[string]any{"description": projectName + " cloud"}, hdr)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return map[string]any{"project": urlID, "stage": "cloud", "error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
	}

	code, raw, err = doJSON(ctx, http.MethodGet,
		fmt.Sprintf("https://meoo.com/open/v1/cli-compat/projects/%s/cloud/environment", urlID), nil, hdr)
	if err != nil {
		return nil, err
	}
	var env struct {
		Data struct {
			MeooProjectAPIKey string `json:"meooProjectApiKey"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	if code != 200 || env.Data.MeooProjectAPIKey == "" {
		return map[string]any{"project": urlID, "stage": "environment", "error": fmt.Sprintf("HTTP %d %s", code, strings.TrimSpace(string(raw)))}, nil
	}
	k := NewKey("账号 "+urlID, env.Data.MeooProjectAPIKey, 1)
	if acc != nil {
		k.Account = &Account{ID: acc.ID, Name: acc.Name, Kind: "key"}
	}
	s.mu.Lock()
	s.Keys = append(s.Keys, k)
	s.mu.Unlock()
	s.Touch()
	_ = s.Save()
	return map[string]any{
		"project": urlID, "keyId": k.ID, "key": Mask(k.Key), "ok": true,
	}, nil
}
