package account

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const quotaBody = `{"success":false,"errCode":"QUOTA_EXCEEDED","message":"云服务实例数已达上限，请提醒升级套餐或释放实例后再试。不要进行不断地重复调用。"}`
const disabledEnvironment = `{"success":true,"data":{"enabled":false,"instance":null}}`

func TestBusinessErrorPreservesActualQuota(t *testing.T) {
	s := newService(t, func(*http.Request) (*http.Response, error) { return reply(200, quotaBody) })
	code, _, err := s.request(context.Background(), http.MethodPost, "https://meoo.com/mock", nil, nil)
	var business *BusinessError
	if code != 200 || !errors.As(err, &business) || business.Code != "QUOTA_EXCEEDED" || business.Message != "云服务实例数已达上限，请提醒升级套餐或释放实例后再试。不要进行不断地重复调用。" {
		t.Fatalf("lost business response: %d %+v %v", code, business, err)
	}
}
func TestDisabledEnvironmentAttemptsCloudOnceThenReturnsActualQuota(t *testing.T) {
	var envs, clouds, lists, creates int
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/open/v1/cli-compat/projects/existing/cloud/environment":
			envs++
			return reply(200, disabledEnvironment)
		case r.URL.Path == "/open/v1/cli-compat/projects/existing/cloud" && r.Method == "POST":
			clouds++
			return reply(200, quotaBody)
		case r.URL.Path == "/open/v1/cli-compat/projects" && r.Method == "GET":
			lists++
			return reply(200, `{"success":true,"data":{"list":[],"hasMore":false}}`)
		default:
			creates++
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			return reply(500, `{}`)
		}
	})
	a := seed(s, "")
	a.ProjectID = "existing"
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := s.ProjectKeyContext(ctx, a, "ignored")
	var business *BusinessError
	if !errors.As(err, &business) || business.Code != "QUOTA_EXCEEDED" || out["stage"] != "cloud" {
		t.Fatalf("quota result %v %v", out, err)
	}
	if envs != 1 || clouds != 1 || lists != 1 || creates != 0 || a.ProjectID != "existing" || len(s.St.Data.Keys) != 0 {
		t.Fatalf("counts env=%d cloud=%d list=%d create=%d", envs, clouds, lists, creates)
	}
}
func TestQuotaReusesOnlyActiveSameUserProject(t *testing.T) {
	var envs, clouds, lists int
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/cli-compat/projects/new/cloud":
			clouds++
			return reply(200, quotaBody)
		case "/open/v1/cli-compat/projects":
			if r.Method != "GET" {
				t.Fatal("fallback created a remote project")
			}
			lists++
			if r.URL.Query().Get("page") != "1" || r.URL.Query().Get("page_size") != "20" {
				t.Error("discovery not bounded")
			}
			return reply(200, `{"success":true,"data":{"list":[
    {"url_id":"other-user","creator_user_id":"other","custom_conf":{"service_access":{"enabled":true,"ak_status":"active"}}},
    {"url_id":"inactive","creator_user_id":"user","custom_conf":{"service_access":{"enabled":true,"ak_status":"inactive"}}},
    {"url_id":"disabled","creator_user_id":"user","custom_conf":{"service_access":{"enabled":false,"ak_status":"active"}}},
    {"url_id":"existing-active","creator_user_id":"user","custom_conf":{"service_access":{"enabled":true,"ak_status":"active"}}}
   ],"hasMore":true,"nextCursor":"unused"}}`)
		case "/open/v1/cli-compat/projects/existing-active/cloud/environment":
			envs++
			return reply(200, `{"success":true,"data":{"enabled":true,"envMode":"production","environment":{},"instance":{"sbInfo":{}},"meooProjectApiKey":"existing-project-ak"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			return reply(500, `{}`)
		}
	})
	a := seed(s, "")
	a.ProjectID = "new"
	// For a saved disabled project, one environment read precedes the quota POST.
	underlying := s.Up.HTTP.Transport
	s.Up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/open/v1/cli-compat/projects/new/cloud/environment" {
			return reply(200, disabledEnvironment)
		}
		return underlying.RoundTrip(r)
	})
	out, err := s.ProjectKey(a, "ignored")
	if err != nil || out["ok"] != true || out["project"] != "existing-active" || a.ProjectID != "existing-active" || len(s.St.Data.Keys) != 1 || s.St.Data.Keys[0].Key != "existing-project-ak" || envs != 1 || lists != 1 || clouds != 1 {
		t.Fatalf("fallback: %v %v env=%d list=%d cloud=%d", out, err, envs, lists, clouds)
	}
	if _, err = s.ProjectKey(a, "again"); err != nil || clouds != 1 || lists != 1 {
		t.Fatal("reused project triggered discovery/provision again")
	}
}
func TestNewProjectQuotaFallbackFailurePreservesProject(t *testing.T) {
	var creates, clouds, lists int
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/open/v1/cli-compat/projects" && r.Method == "POST":
			creates++
			return reply(200, `{"success":true,"data":{"url_id":"created"}}`)
		case r.URL.Path == "/open/v1/cli-compat/projects/created/cloud":
			clouds++
			return reply(200, quotaBody)
		case r.URL.Path == "/open/v1/cli-compat/projects" && r.Method == "GET":
			lists++
			return reply(503, `{}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			return reply(500, `{}`)
		}
	})
	a := seed(s, "")
	out, err := s.ProjectKey(a, "new")
	var business *BusinessError
	if !errors.As(err, &business) || business.Code != "QUOTA_EXCEEDED" || out["project"] != "created" || a.ProjectID != "created" || creates != 1 || clouds != 1 || lists != 1 {
		t.Fatalf("fallback lost original quota or project: %v %v", out, err)
	}
}
func TestQuotaDiscoveryCapsOnePageAndTenCandidates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefix   int
		wantEnvs int
	}{{"candidate cap", 0, 10}, {"page cap", 20, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			var envs, lists int
			s := newService(t, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/open/v1/cli-compat/projects" {
					lists++
					items := []any{}
					for i := 0; i < tc.prefix; i++ {
						items = append(items, map[string]any{"url_id": fmt.Sprint("other-", i), "creator_user_id": "other"})
					}
					for i := 0; i < 12; i++ {
						items = append(items, map[string]any{"url_id": fmt.Sprint("candidate-", i), "creator_user_id": "user", "custom_conf": map[string]any{"service_access": map[string]any{"enabled": true, "ak_status": "active"}}})
					}
					raw, _ := json.Marshal(map[string]any{"success": true, "data": map[string]any{"list": items, "hasMore": true, "nextCursor": "do-not-follow"}})
					return reply(200, string(raw))
				}
				if !strings.HasSuffix(r.URL.Path, "/cloud/environment") || r.Method != "GET" {
					t.Errorf("fallback mutated remote project %s %s", r.Method, r.URL)
				}
				envs++
				return reply(200, disabledEnvironment)
			})
			_, _, err := s.reusableProject(context.Background(), "user", nil)
			if err == nil || envs != tc.wantEnvs || lists != 1 {
				t.Fatalf("unbounded discovery: env=%d list=%d err=%v", envs, lists, err)
			}
		})
	}
}
func TestBusinessFailureOnUnauthorizedStatusStillRotatesCredential(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			return reply(401, `{"success":false,"errCode":"TOKEN_EXPIRED","message":"expired"}`)
		}
		return reply(200, `{"data":{"apiKey":"replacement"}}`)
	})
	a := seed(s, "cookie")
	pk, err := s.PersonalKey(a)
	if err != nil || pk.Key != "replacement" {
		t.Fatalf("business wrapper prevented rotation: %v %v", pk, err)
	}
	s.Up.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return reply(403, `{"success":false,"message":"expired"}`)
	})
	if _, err := s.Ping(a); err == nil || !a.Expired {
		t.Fatal("HTTP auth failure no longer expired session")
	}
}
