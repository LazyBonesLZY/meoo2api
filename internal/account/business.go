package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// BusinessError preserves an explicit upstream business rejection separately
// from HTTP/transport failures. In particular HTTP 200 does not mean success.
type BusinessError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *BusinessError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	if e.Code != "" {
		return e.Code
	}
	return "上游操作未成功"
}

var errCloudDisabled = errors.New("项目云服务尚未开通")

// reusableProject only reads one bounded page after a quota rejection. It never
// provisions, modifies or removes any remote project, and never switches users.
func (s *Service) reusableProject(ctx context.Context, userID string, hdr map[string]string) (string, string, error) {
	if userID == "" {
		return "", "", fmt.Errorf("账号身份为空")
	}
	code, raw, err := s.request(ctx, http.MethodGet, "https://meoo.com/open/v1/cli-compat/projects?page=1&page_size=20", nil, hdr)
	if err != nil {
		return "", "", err
	}
	var projects struct {
		Data struct {
			List []struct {
				URLID   string `json:"url_id"`
				Creator string `json:"creator_user_id"`
				Config  struct {
					Access struct {
						Enabled bool   `json:"enabled"`
						Status  string `json:"ak_status"`
					} `json:"service_access"`
				} `json:"custom_conf"`
			} `json:"list"`
		} `json:"data"`
	}
	if code != 200 || json.Unmarshal(raw, &projects) != nil {
		return "", "", fmt.Errorf("读取已有项目失败 HTTP %d", code)
	}
	candidates := 0
	for index, p := range projects.Data.List {
		if index >= 20 || candidates >= 10 {
			break
		}
		if p.URLID == "" || p.Creator != userID || !p.Config.Access.Enabled || p.Config.Access.Status != "active" {
			continue
		}
		candidates++
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		code, raw, err := s.request(ctx, http.MethodGet, "https://meoo.com/open/v1/cli-compat/projects/"+url.PathEscape(p.URLID)+"/cloud/environment", nil, hdr)
		if err != nil {
			continue
		}
		var env struct {
			Data struct {
				Enabled *bool  `json:"enabled"`
				Key     string `json:"meooProjectApiKey"`
			} `json:"data"`
		}
		if code == 200 && json.Unmarshal(raw, &env) == nil && env.Data.Enabled != nil && *env.Data.Enabled && env.Data.Key != "" {
			return p.URLID, env.Data.Key, nil
		}
	}
	return "", "", fmt.Errorf("本页没有可复用的已开通项目")
}
