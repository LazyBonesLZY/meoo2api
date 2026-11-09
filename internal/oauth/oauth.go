// Package oauth 处理 Meoo OAuth（PKCE + refresh），为账号提供免浏览器的长期凭证。
package oauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	ClientID  = "meoo_app_tdWlRbpvAxN1EjmZ8XICxdTh"
	TokenURL  = "https://meoo.com/oauth/token"
	Authorize = "https://meoo.com/oauth/authorize"
	Scope     = "cli.compat"
)

type Token struct {
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	Expires int64  `json:"expires_in"`
	Scope   string `json:"scope"`
	OpenID  string `json:"openid"`
}

func post(ctx context.Context, form url.Values) (*Token, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var t Token
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 || t.Access == "" {
		return nil, fmt.Errorf("token 接口 HTTP %d", resp.StatusCode)
	}
	return &t, nil
}

// Refresh 用 refresh_token 换新 access_token（refresh 会轮换）
func Refresh(ctx context.Context, refresh string) (*Token, error) {
	return post(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {ClientID},
		"refresh_token": {refresh},
	})
}

// Exchange 用授权码换 token
func Exchange(ctx context.Context, code, redirectURI, verifier string) (*Token, error) {
	return post(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {ClientID},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
}
