package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int, text string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(text)), Header: make(http.Header)}
}

func fixture(t *testing.T) (*Handler, string) {
	t.Helper()
	st, _, err := store.Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Data.Config.AdminPass = model.ShaHex("old-password-123")
	up := upstream.New("https://upstream.invalid/v1")
	up.HTTP = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		t.Errorf("unexpected outbound request: %s", r.URL)
		return response(500, `{}`), nil
	})}
	h := &Handler{St: st, Tokens: NewTokens(), Acc: &account.Service{St: st, Up: up}, Picker: &pool.Picker{St: st, Up: up}, Start: time.Now()}
	token, err := h.Tokens.issue()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.forgetDevices()
		if err := st.Close(); err != nil {
			t.Error(err)
		}
	})
	return h, token
}
func request(h *Handler, token, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "http://console.test"+path, strings.NewReader(body))
	r.RemoteAddr = "192.0.2.8:1234"
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	if !h.ServeHTTP(w, r) {
		panic("admin path not handled")
	}
	return w
}
func expect(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
}
func object(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestAccountQuotaBusinessError(t *testing.T) {
	h, _ := fixture(t)
	const message = "Meoo 云服务实例数已达上限，释放已有实例后重试"
	quota := &account.BusinessError{Code: "QUOTA_EXCEEDED", Message: "云服务实例数已达上限 raw-secret-cookie"}
	for _, err := range []error{quota, fmt.Errorf("开通云服务失败: %w", quota)} {
		if got := accountError(err, map[string]any{"stage": "cloud"}); got != message {
			t.Fatalf("quota condition lost: %q", got)
		}
		w := httptest.NewRecorder()
		h.accountResult(w, nil, map[string]any{"stage": "cloud"}, err)
		expect(t, w, 400)
		if got := object(t, w)["error"]; got != message {
			t.Fatalf("quota response lost: %v", got)
		}
		if strings.Contains(w.Body.String(), "raw-secret-cookie") {
			t.Fatal("raw upstream detail leaked")
		}
	}
	unknown := &account.BusinessError{Code: "OTHER_ERROR", Message: "raw-secret-cookie"}
	if got := accountError(unknown, map[string]any{"stage": "cloud"}); strings.Contains(got, "raw-secret-cookie") || got == message {
		t.Fatalf("unknown business error incorrectly rendered: %q", got)
	}
	if got := accountError(errors.New("开通云服务失败 HTTP 429"), nil); got == message {
		t.Fatal("HTTP status alone was misclassified as quota")
	}
}

func TestLoginMigrationRevocationAndRateLimit(t *testing.T) {
	h, _ := fixture(t)
	w := request(h, "", "POST", "/admin/api/login", `{"username":"admin","password":"old-password-123"}`)
	expect(t, w, 200)
	token := object(t, w)["token"].(string)
	if len(token) != 64 || len(h.St.Data.Config.AdminPass) == 64 {
		t.Fatal("session strength or legacy password upgrade missing")
	}
	for _, auth := range []string{token, "admin:" + token, "Bearer admin:" + token, "Basic " + token} {
		expect(t, request(h, "", "GET", "/admin/api/state", "", "Authorization", auth), 401)
	}
	expect(t, request(h, token, "GET", "/admin/api/state", ""), 200)
	expect(t, request(h, token, "POST", "/admin/api/logout", ""), 200)
	expect(t, request(h, token, "GET", "/admin/api/state", ""), 401)
	for i := 0; i < 9; i++ {
		expect(t, request(h, "", "POST", "/admin/api/login", `{"password":"wrong"}`, "X-Forwarded-For", "192.0.2."+string(rune('a'+i))), 401)
	}
	w = request(h, "", "POST", "/admin/api/login", `{"password":"wrong"}`)
	expect(t, w, 429)
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

func TestSameOriginAndMethods(t *testing.T) {
	h, token := fixture(t)
	for _, headers := range [][]string{{"Origin", "https://evil.test"}, {"Origin", "null"}, {"Sec-Fetch-Site", "cross-site"}, {"Origin", "http://console.test/path"}} {
		expect(t, request(h, token, "GET", "/admin/api/state", "", headers...), 403)
	}
	expect(t, request(h, token, "GET", "/admin/api/state", "", "Origin", "https://console.test"), 200)
	for _, x := range []struct {
		method, path string
		code         int
	}{
		{"GET", "/admin/api/login", 405}, {"DELETE", "/admin/api/accounts", 405}, {"PUT", "/admin/api/api-keys", 405},
		{"POST", "/admin/api/state", 405}, {"GET", "/admin/api/config", 405}, {"GET", "/admin/api/export", 200},
	} {
		expect(t, request(h, token, x.method, x.path, "{}"), x.code)
	}
}

func TestStrictJSONAndAtomicConfig(t *testing.T) {
	h, token := fixture(t)
	original := h.St.Data.Config.TimeoutMs
	for _, body := range []string{"", `null`, `[]`, `{} {}`, `{"timeoutMs":2000,"unexpected":true}`, `{"TimeoutMs":2000}`, `{"timeoutMs":2000,"timeoutMs":3000}`, `{"timeoutMs":null}`, `{"timeoutMs":1234.5}`, `{"timeoutMs":0}`, `{"timeoutMs":2000,"checkinHour":24}`, `{"keepAliveHours":169}`, `{"adminPassword":"short"}`} {
		expect(t, request(h, token, "PATCH", "/admin/api/config", body), 400)
		if h.St.Data.Config.TimeoutMs != original {
			t.Fatalf("partial config update for %q", body)
		}
	}
	expect(t, request(h, token, "PATCH", "/admin/api/config", `{"sticky":true}`, "Content-Type", "text/plain"), 415)
	expect(t, request(h, token, "PATCH", "/admin/api/config", `{"adminPassword":"`+strings.Repeat("a", 65536)+`"}`), 413)
	expect(t, request(h, token, "PATCH", "/admin/api/config", `{"timeoutMs":2500,"keepAliveHours":0,"defaultMaxTokens":0}`), 200)
	if h.St.Data.Config.TimeoutMs != 2500 || h.St.Data.Config.KeepAliveHours != 0 || h.St.Data.Config.DefaultMaxTokens != 0 {
		t.Fatal("valid boundary values lost")
	}
	expect(t, request(h, token, "PATCH", "/admin/api/config", `{"adminPassword":"new-long-password"}`), 200)
	expect(t, request(h, token, "GET", "/admin/api/state", ""), 401)
	if !model.VerifyPassword(h.St.Data.Config.AdminPass, "new-long-password") {
		t.Fatal("password update missing")
	}
}

func seedAccount(h *Handler) {
	h.St.Data.Accounts = []*model.Account{{ID: "a_1", UserID: "user-1", Name: `"><img src=x onerror=alert(1)>`, Cookie: "tiny", Enabled: true, KeyID: "k_1", ProjectKeyIDs: []string{"k_1", "k_2"}, Last: &model.CheckinLast{Error: "secret-cookie"}}}
	h.St.Data.Keys = []*model.Key{{ID: "k_1", Key: "tiny-key", AccountID: "a_1", Enabled: true}, {ID: "k_2", Key: "upstream-very-secret-credential-123", AccountID: "a_1", Enabled: true}, {ID: "orphan", Key: "orphan-secret", Enabled: true}}
	h.St.Data.ClientKeys = []*model.ClientKey{{ID: "c_1", Key: "client-secret", Enabled: true}}
	h.St.Data.PersonalKeys = []*model.PersonalKey{{ID: "pk_1", AccountID: "a_1", Key: "personal-secret"}, {ID: "pk_legacy", AccountID: "user-1", Key: "legacy-personal"}}
}
func TestRedactionAndRevealBoundaries(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	for _, path := range []string{"/admin/api/state", "/admin/api/export", "/admin/api/api-keys"} {
		w := request(h, token, "GET", path, "")
		expect(t, w, 200)
		for _, secret := range []string{"tiny-key", "client-secret", "orphan-secret", "personal-secret", "legacy-personal", "secret-cookie", h.St.Data.Config.AdminPass} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("%s leaked %q", path, secret)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
	}
	w := request(h, token, "GET", "/admin/api/accounts/a_1/key?cookie=1", "")
	expect(t, w, 200)
	if object(t, w)["key"] != "tiny-key" || strings.Contains(w.Body.String(), "cookie") {
		t.Fatal("reveal boundary changed")
	}
	expect(t, request(h, token, "DELETE", "/admin/api/api-keys/c_1/reveal", ""), 405)
	expect(t, request(h, token, "PATCH", "/admin/api/accounts/a_1/probe", `{"enabled":false}`), 405)
	expect(t, request(h, token, "DELETE", "/admin/api/accounts/a_1/key", ""), 405)
}
func TestAccountAndOrphanLifecycle(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	expect(t, request(h, token, "PATCH", "/admin/api/accounts/a_1", `{"enabled":false}`), 200)
	for _, k := range h.St.Data.Keys[:2] {
		if k.Enabled {
			t.Fatal("disabled account key still enabled")
		}
	}
	expect(t, request(h, token, "PATCH", "/admin/api/accounts/a_1", `{"enabled":true,"cookie":""}`), 200)
	if h.St.Data.Accounts[0].Cookie != "" {
		t.Fatal("Cookie detachment missing")
	}
	expect(t, request(h, token, "PATCH", "/admin/api/orphan-keys/orphan", `{"enabled":false}`), 200)
	if h.St.Data.Keys[2].Enabled {
		t.Fatal("orphan disable failed")
	}
	expect(t, request(h, token, "DELETE", "/admin/api/orphan-keys/k_1", ""), 404)
	expect(t, request(h, token, "DELETE", "/admin/api/accounts/a_1", ""), 200)
	if len(h.St.Data.Accounts) != 0 || len(h.St.Data.PersonalKeys) != 0 || len(h.St.Data.Keys) != 1 {
		t.Fatal("account credentials not fully deleted")
	}
	expect(t, request(h, token, "DELETE", "/admin/api/orphan-keys/orphan", ""), 200)
	if len(h.St.Data.Keys) != 0 {
		t.Fatal("orphan delete failed")
	}
}
func TestSupplementCookieValidatesIdentityAndKeepsCredentials(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	a := h.St.Data.Accounts[0]
	a.Cookie = ""
	a.Expired = true
	var validations int
	h.Acc.Up.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		validations++
		if r.Method != "GET" || r.URL.Path != "/api/v1/user/me" || r.Header.Get("Authorization") != "" {
			t.Errorf("Cookie supplement performed extra operation: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Cookie") == "session=other" {
			return response(200, `{"success":true,"data":{"user_id":"another-user","name":"Other"}}`), nil
		}
		return response(200, `{"success":true,"data":{"user_id":"user-1","name":"Alice"}}`), nil
	})
	w := request(h, token, "PATCH", "/admin/api/accounts/a_1", `{"cookie":"session=correct"}`)
	expect(t, w, 200)
	if a.Cookie != "session=correct" || a.Expired || !a.PingOK || a.KeyID != "k_1" || a.Last != nil {
		t.Fatalf("Cookie supplement did not update login state: %+v", a)
	}
	if len(h.St.Data.Accounts) != 1 || len(h.St.Data.Keys) != 3 || len(h.St.Data.PersonalKeys) != 2 {
		t.Fatal("Cookie supplement created accounts or credentials")
	}
	w = request(h, token, "GET", "/admin/api/state", "")
	expect(t, w, 200)
	if strings.Contains(w.Body.String(), "session=correct") || object(t, w)["accounts"].([]any)[0].(map[string]any)["hasCookie"] != true {
		t.Fatal("state leaked Cookie or missing binding indicator")
	}
	w = request(h, token, "PATCH", "/admin/api/accounts/a_1", `{"cookie":"session=other"}`)
	expect(t, w, 400)
	if a.Cookie != "session=correct" || a.UserID != "user-1" || validations != 2 {
		t.Fatal("mismatched Cookie replaced current login state")
	}
}

func TestInvalidCookieAddNeverPanicsOrPersists(t *testing.T) {
	h, token := fixture(t)
	h.Acc.Up.HTTP.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return response(401, `{"error":{"message":"Cookie=LEAK"}}`), nil
	})
	w := request(h, token, "POST", "/admin/api/accounts", `{"cookie":"invalid-cookie"}`)
	expect(t, w, 400)
	if len(h.St.Data.Accounts) != 0 || strings.Contains(w.Body.String(), "LEAK") {
		t.Fatal("invalid identity persisted or raw error leaked")
	}
}
func TestStateUsageAndExportConcurrentSnapshot(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	h.St.Data.Usage["2026-01-01T00"] = &model.Bucket{Models: map[string]int64{}, Lat: []float64{1}}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			h.St.Mu.Lock()
			h.St.Data.Config.Aliases["test"] = "value"
			h.St.Data.Usage["2026-01-01T00"].Models["test"]++
			h.St.Data.Accounts[0].Last.Amount++
			h.St.Mu.Unlock()
		}
	}()
	for i := 0; i < 30; i++ {
		for _, path := range []string{"/admin/api/state", "/admin/api/export", "/admin/api/usage"} {
			expect(t, request(h, token, "GET", path, ""), 200)
		}
	}
	wg.Wait()
}
func TestDeviceOwnershipConcurrentPollAndCancellation(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	var polls atomic.Int32
	h.Acc.Up.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v1/cli/auth/session":
			return response(200, `{"data":{"sessionId":"device-session-1"}}`), nil
		case strings.HasPrefix(r.URL.Path, "/api/v1/cli/auth/session/"):
			polls.Add(1)
			return response(200, `{"data":{"status":"approved","apiKey":"device-personal-secret","userId":"user-1"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/account/identity"):
			return response(200, `{"data":{"user_id":"user-1","name":"Device User"}}`), nil
		case strings.HasSuffix(r.URL.Path, "/account/points"):
			return response(200, `{"data":{"account":{"availableBalance":42}}}`), nil
		default:
			t.Errorf("unexpected network path %s", r.URL)
			return response(500, `{}`), nil
		}
	})
	w := request(h, token, "POST", "/admin/api/device/start", "{}")
	expect(t, w, 200)
	sid := object(t, w)["sessionId"].(string)
	other, _ := h.Tokens.issue()
	expect(t, request(h, other, "GET", "/admin/api/device/poll?sid="+sid, ""), 404)
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- request(h, token, "GET", "/admin/api/device/poll?sid="+sid, "") }()
	}
	wg.Wait()
	close(results)
	for result := range results {
		expect(t, result, 200)
		if object(t, result)["status"] != "approved" || strings.Contains(result.Body.String(), "device-personal-secret") {
			t.Fatal("bad cached device result")
		}
	}
	if polls.Load() != 1 {
		t.Fatalf("approved session consumed %d times", polls.Load())
	}
	expect(t, request(h, token, "POST", "/admin/api/device/cancel", ""), 200)
	expect(t, request(h, token, "GET", "/admin/api/device/poll?sid="+sid, ""), 404)
}
func TestCancelPendingDeviceStart(t *testing.T) {
	h, token := fixture(t)
	started := make(chan struct{})
	h.Acc.Up.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		close(started)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- request(h, token, "POST", "/admin/api/device/start", "{}") }()
	<-started
	expect(t, request(h, token, "POST", "/admin/api/device/cancel", ""), 200)
	select {
	case w := <-result:
		if w.Code == 200 {
			t.Fatal("cancelled start succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("device start ignored cancellation")
	}
	h.deviceMu.Lock()
	defer h.deviceMu.Unlock()
	if len(h.devices) > 0 || len(h.pendingDevices) > 0 {
		t.Fatal("cancelled start retained session")
	}
}
func TestSessionExpiryBoundAndTokenLimit(t *testing.T) {
	h, token := fixture(t)
	h.Tokens.mu.Lock()
	h.Tokens.data[token] = time.Now().Add(-time.Second)
	h.Tokens.mu.Unlock()
	expect(t, request(h, token, "GET", "/admin/api/state", ""), 401)
	for i := 0; i < 300; i++ {
		if _, err := h.Tokens.issue(); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.Tokens.data) != 256 {
		t.Fatal("unbounded session storage")
	}
	token, _ = h.Tokens.issue()
	h.Tokens.mu.Lock()
	h.Tokens.limits["api:"+token] = rateWindow{start: time.Now(), count: 600}
	h.Tokens.mu.Unlock()
	expect(t, request(h, token, "GET", "/admin/api/state", ""), 429)
}
