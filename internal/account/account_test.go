package account

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}
func newService(t *testing.T, fn roundTripFunc) *Service {
	t.Helper()
	st, _, err := store.Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := any(st).(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
	return &Service{St: st, Up: &upstream.Client{HTTP: &http.Client{Transport: fn}}}
}
func seed(s *Service, cookie string) *model.Account {
	a := &model.Account{ID: "a_test", UserID: "user", Cookie: cookie, Enabled: true, PersonalKeyIDs: []string{"personal"}}
	s.St.Data.Accounts = []*model.Account{a}
	s.St.Data.PersonalKeys = []*model.PersonalKey{{ID: "personal", AccountID: a.ID, Key: "personal-secret"}}
	return a
}
func TestAddCookieConcurrentIsIdempotent(t *testing.T) {
	var projects, personal atomic.Int32
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/cli-compat/account/points":
			return reply(200, `{"data":{"account":{"availableBalance":42}}}`)
		case "/api/v1/user/me":
			if r.Header.Get("Cookie") == "" || r.Header.Get("Authorization") != "" {
				t.Error("cookie identity must not use an empty bearer")
			}
			return reply(200, `{"data":{"user_id":"user","name":"mock"}}`)
		case "/api/v1/user/api-keys":
			personal.Add(1)
			return reply(200, `{"data":{"apiKey":"personal-secret"}}`)
		case "/open/v1/cli-compat/projects":
			projects.Add(1)
			return reply(200, `{"data":{"url_id":"project"}}`)
		case "/open/v1/cli-compat/projects/project/cloud":
			return reply(200, `{}`)
		case "/open/v1/cli-compat/projects/project/cloud/environment":
			return reply(200, `{"data":{"meooProjectApiKey":"project-secret"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			return reply(500, `{}`)
		}
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, out, err := s.Add("session=mock")
			if err != nil || a == nil || out["ok"] != true {
				t.Errorf("add: %v %v", out, err)
			}
		}()
	}
	wg.Wait()
	if projects.Load() != 1 || personal.Load() != 1 {
		t.Fatalf("projects=%d personal=%d", projects.Load(), personal.Load())
	}
	if len(s.St.Data.Accounts) != 1 || len(s.St.Data.Keys) != 1 || len(s.St.Data.PersonalKeys) != 1 {
		t.Fatal("duplicate account/key")
	}
}
func TestAddInvalidCookieDoesNotPersist(t *testing.T) {
	s := newService(t, func(*http.Request) (*http.Response, error) { return reply(401, `{"error":{"message":"secret"}}`) })
	if a, _, err := s.Add("bad"); err == nil || a != nil {
		t.Fatal("invalid cookie accepted")
	}
	if len(s.St.Data.Accounts) != 0 {
		t.Fatal("invalid account was persisted")
	}
}
func TestProjectRetryReusesPersistedProject(t *testing.T) {
	var projects, cloud atomic.Int32
	var healthy atomic.Bool
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/cli-compat/projects":
			projects.Add(1)
			return reply(200, `{"data":{"url_id":"saved-project"}}`)
		case "/open/v1/cli-compat/projects/saved-project/cloud":
			if cloud.Add(1) == 1 {
				return reply(503, `{}`)
			}
			healthy.Store(true)
			return reply(200, `{}`)
		case "/open/v1/cli-compat/projects/saved-project/cloud/environment":
			if !healthy.Load() {
				return reply(404, `{}`)
			}
			return reply(200, `{"data":{"meooProjectApiKey":"project-secret"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			return reply(500, `{}`)
		}
	})
	a := seed(s, "")
	out, err := s.ProjectKey(a, "name")
	if err == nil || out["stage"] != "cloud" || a.ProjectID != "saved-project" {
		t.Fatalf("first: %v %v", out, err)
	}
	out, err = s.ProjectKey(a, "another-name")
	if err != nil || out["ok"] != true {
		t.Fatalf("retry: %v %v", out, err)
	}
	if projects.Load() != 1 || cloud.Load() != 2 {
		t.Fatal("retry recreated project")
	}
	if _, err = s.ProjectKey(a, "again"); err != nil || projects.Load() != 1 {
		t.Fatal("existing pool key not reused")
	}
}
func TestDeviceConcurrentConsumesAndOnboardsOnce(t *testing.T) {
	var polls, projects atomic.Int32
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/open/v1/cli-compat/account/points":
			return reply(200, `{"data":{"account":{"availableBalance":42}}}`)
		case "/api/v1/cli/auth/session":
			return reply(200, `{"success":true,"data":{"sessionId":"sid"}}`)
		case "/api/v1/cli/auth/session/sid":
			if polls.Add(1) > 1 {
				return reply(200, `{"data":{"status":"expired"}}`)
			}
			return reply(200, `{"data":{"status":"approved","apiKey":"device-secret","userId":"user"}}`)
		case "/open/v1/cli-compat/account/identity":
			if r.Header.Get("Authorization") != "Bearer device-secret" {
				t.Error("wrong AK")
			}
			return reply(200, `{"data":{"user_id":"user","name":"mock"}}`)
		case "/open/v1/cli-compat/projects":
			projects.Add(1)
			return reply(200, `{"data":{"url_id":"project"}}`)
		case "/open/v1/cli-compat/projects/project/cloud":
			return reply(200, `{}`)
		case "/open/v1/cli-compat/projects/project/cloud/environment":
			return reply(200, `{"data":{"meooProjectApiKey":"project-secret"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			return reply(500, `{}`)
		}
	})
	sid, _, err := s.StartDevice()
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := s.PollDevice(sid)
			if err != nil {
				t.Error(err)
				return
			}
			raw, _ := json.Marshal(st)
			if strings.Contains(string(raw), "device-secret") {
				t.Error("credential exposed")
			}
			a, _, err := s.AddFromDevice(st)
			if err != nil || a == nil {
				t.Errorf("device add: %v", err)
			}
		}()
	}
	wg.Wait()
	if polls.Load() != 1 || projects.Load() != 1 || len(s.St.Data.PersonalKeys) != 1 {
		t.Fatalf("polls=%d projects=%d pks=%d", polls.Load(), projects.Load(), len(s.St.Data.PersonalKeys))
	}
	if s.St.Data.PersonalKeys[0].AccountID != s.St.Data.Accounts[0].ID {
		t.Fatal("personal key not bound to canonical account id")
	}
	if _, err = s.PollDevice("not-started"); err == nil {
		t.Fatal("arbitrary sid accepted")
	}
	s.deviceMu.Lock()
	s.devices[sid].expires = time.Now().Add(-time.Second)
	s.deviceMu.Unlock()
	if _, err = s.PollDevice(sid); err == nil {
		t.Fatal("expired local session accepted")
	}
}
func TestNewestDeviceCredentialIsUsedAndDeduplicated(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			return reply(200, `{"data":{"user_id":"user","name":"mock"}}`)
		}
		if strings.HasSuffix(r.URL.Path, "/points") {
			return reply(200, `{"data":{"account":{"availableBalance":42}}}`)
		}
		t.Errorf("unexpected request: %s", r.URL.Path)
		return reply(500, `{}`)
	})
	a := seed(s, "browser")
	a.Expired = true
	a.KeyID = "key"
	s.St.Data.Keys = []*model.Key{{ID: "key", AccountID: a.ID, Key: "project", Enabled: true}}
	for i := 0; i < 2; i++ {
		if _, _, err := s.AddFromDevice(&DeviceState{Status: "approved", APIKey: "new-secret", UserID: "user"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := s.personalAKOf(a); got != "new-secret" {
		t.Fatalf("selected stale AK %q", got)
	}
	if len(s.St.Data.PersonalKeys) != 2 {
		t.Fatal("repeated import duplicated credential")
	}
	if !a.Expired {
		t.Fatal("device AK revived expired Cookie")
	}
	pks := s.PersonalKeyOf(a)
	pks[0].Key = "mutated"
	if s.St.Data.PersonalKeys[0].Key == "mutated" {
		t.Fatal("mutable credential pointer exposed")
	}
}
func TestPingClassifiesFailuresAndProtectsReplacement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		code        int
		body        string
		wantExpired bool
	}{{"transient", 503, `{}`, false}, {"throttle", 429, `{}`, false}, {"malformed", 200, `{`, false}, {"expired", 401, `{}`, true}, {"identity mismatch", 200, `{"data":{"user_id":"other"}}`, true}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newService(t, func(*http.Request) (*http.Response, error) { return reply(tc.code, tc.body) })
			a := seed(s, "cookie")
			if _, err := s.Ping(a); err == nil {
				t.Fatal("failure accepted")
			}
			if a.Expired != tc.wantExpired {
				t.Fatalf("expired=%v", a.Expired)
			}
		})
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s := newService(t, func(r *http.Request) (*http.Response, error) { close(entered); <-release; return reply(401, `{}`) })
	a := seed(s, "old")
	done := make(chan struct{})
	go func() { defer close(done); _, _ = s.Ping(a) }()
	<-entered
	s.St.Mu.Lock()
	a.Cookie = "new"
	s.St.Mu.Unlock()
	close(release)
	<-done
	if a.Expired {
		t.Fatal("old response poisoned replacement")
	}
}
func TestDeviceOnlyPingAndMalformedPoints(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Cookie") != "" {
			t.Error("device-only ping sent Cookie")
		}
		if strings.HasSuffix(r.URL.Path, "/identity") {
			return reply(200, `{"data":{"user_id":"user"}}`)
		}
		return reply(200, `{"data":{}}`)
	})
	a := seed(s, "")
	a.Expired = true
	a.Points = 123
	if ok, err := s.Ping(a); !ok || err != nil || a.Expired {
		t.Fatalf("device ping failed: %v", err)
	}
	if _, err := s.Points(a); err == nil || a.Points != 123 {
		t.Fatal("malformed points overwrote stored balance")
	}
}
func TestDeleteWaitsForOnboardAndRemovesAllCredentials(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			return reply(200, `{"data":{"user_id":"user"}}`)
		}
		if r.URL.Path == "/open/v1/cli-compat/projects" {
			close(entered)
			<-release
			return reply(200, `{"data":{"url_id":"project"}}`)
		}
		return reply(200, `{"data":{"meooProjectApiKey":"new-project"}}`)
	})
	a := seed(s, "")
	onboardDone := make(chan error, 1)
	go func() { _, err := s.Onboard(a); onboardDone <- err }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.DeleteContext(ctx, a.ID); !errors.Is(err, context.Canceled) {
		t.Fatal("gate ignored cancellation")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- s.DeleteContext(context.Background(), a.ID) }()
	close(release)
	if err := <-onboardDone; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if len(s.St.Data.Accounts)+len(s.St.Data.Keys)+len(s.St.Data.PersonalKeys) != 0 {
		t.Fatal("orphaned credentials after delete")
	}
	if _, err := s.Onboard(a); err == nil {
		t.Fatal("deleted account resurrected")
	}
}
func TestPersonalKeyReplacesOnlyDefinitivelyRejectedCredential(t *testing.T) {
	var minted atomic.Int32
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/identity") {
			if r.Header.Get("Authorization") == "Bearer personal-secret" {
				return reply(401, `{}`)
			}
			return reply(200, `{"data":{"user_id":"user"}}`)
		}
		minted.Add(1)
		return reply(200, `{"data":{"apiKey":"replacement"}}`)
	})
	a := seed(s, "valid-cookie")
	for i := 0; i < 2; i++ {
		pk, err := s.PersonalKey(a)
		if err != nil || pk.Key != "replacement" {
			t.Fatalf("personal key: %v %v", pk, err)
		}
	}
	if minted.Load() != 1 || len(s.St.Data.PersonalKeys) != 1 {
		t.Fatal("invalid credential retained or replacement duplicated")
	}
	s.Up.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(503, `{}`) })
	if _, err := s.PersonalKey(a); err == nil || s.personalAKOf(a) != "replacement" {
		t.Fatal("transient failure removed valid credential")
	}
}
func TestInvalidProjectKeyRestoresSameProjectAndKeyID(t *testing.T) {
	var calls atomic.Int32
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/open/v1/cli-compat/projects/existing/cloud/environment" {
			t.Errorf("unexpected project recreation: %s %s", r.Method, r.URL.Path)
		}
		return reply(200, `{"data":{"meooProjectApiKey":"rotated"}}`)
	})
	a := seed(s, "")
	a.ProjectID = "existing"
	a.KeyID = "same-id"
	k := &model.Key{ID: a.KeyID, AccountID: a.ID, Key: "old", Health: model.Health{State: "invalid"}, CooldownUntil: 123}
	s.St.Data.Keys = []*model.Key{k}
	out, err := s.ProjectKey(a, "ignored")
	if err != nil || out["keyId"] != "same-id" || len(s.St.Data.Keys) != 1 || k.Key != "rotated" || k.Health.State != "unknown" || k.CooldownUntil != 0 || calls.Load() != 1 {
		t.Fatalf("project recovery: %v %v %+v", out, err, k)
	}
}
func TestEnvironmentTransientDoesNotProvisionAgain(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("transient failure triggered provisioning")
		}
		return reply(503, `{}`)
	})
	a := seed(s, "")
	a.ProjectID = "existing"
	if out, err := s.ProjectKey(a, "ignored"); err == nil || out["stage"] != "environment" {
		t.Fatalf("result: %v %v", out, err)
	}
}
func TestPendingDevicePollIsThrottledAndTerminalStatusCached(t *testing.T) {
	var polls atomic.Int32
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == "POST" {
			return reply(200, `{"data":{"sessionId":"sid"}}`)
		}
		if polls.Add(1) == 1 {
			return reply(200, `{"data":{"status":"pending"}}`)
		}
		return reply(200, `{"data":{"status":"expired"}}`)
	})
	sid, _, err := s.StartDevice()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if state, err := s.PollDevice(sid); err != nil || state.Status != "pending" {
			t.Fatalf("pending %v %v", state, err)
		}
	}
	if polls.Load() != 1 {
		t.Fatal("pending poll storm was not coalesced")
	}
	s.deviceMu.Lock()
	s.devices[sid].nextPoll = time.Time{}
	s.deviceMu.Unlock()
	for i := 0; i < 10; i++ {
		if state, err := s.PollDevice(sid); err != nil || state.Status != "expired" {
			t.Fatalf("terminal %v %v", state, err)
		}
	}
	if polls.Load() != 2 {
		t.Fatal("terminal status was polled again")
	}
}
func TestEnvironmentNotReadyWaitIsCancellable(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			t.Error("not-ready env triggered POST")
		}
		return reply(200, `{"data":{}}`)
	})
	a := seed(s, "")
	a.ProjectID = "existing"
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	out, err := s.ProjectKeyContext(ctx, a, "ignored")
	if !errors.Is(err, context.DeadlineExceeded) || out["stage"] != "environment" {
		t.Fatalf("poll cancellation: %v %v", out, err)
	}
}
func TestExplicitFailureAndRedirectDoNotAcceptIdentity(t *testing.T) {
	for _, body := range []string{`{"success":false,"data":{"user_id":"user"}}`, `{"data":{"user_id":"user"}}`} {
		var calls atomic.Int32
		s := newService(t, func(r *http.Request) (*http.Response, error) {
			calls.Add(1)
			if strings.Contains(body, "false") {
				return reply(200, body)
			}
			resp, _ := reply(302, body)
			resp.Header.Set("Location", "https://meoo.com/another")
			return resp, nil
		})
		if a, _, err := s.Add("cookie"); a != nil || err == nil || calls.Load() != 1 {
			t.Fatal("explicit failure or redirect accepted")
		}
	}
}
func TestPartialDeviceOnboardingCannotReviveDeletedAccount(t *testing.T) {
	s := newService(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/cli/auth/session":
			return reply(200, `{"data":{"sessionId":"sid"}}`)
		case "/api/v1/cli/auth/session/sid":
			return reply(200, `{"data":{"status":"approved","apiKey":"device-secret","userId":"user"}}`)
		case "/open/v1/cli-compat/account/identity":
			return reply(200, `{"data":{"user_id":"user"}}`)
		case "/open/v1/cli-compat/projects":
			return reply(200, `{"data":{"url_id":"existing"}}`)
		case "/open/v1/cli-compat/account/points":
			return reply(200, `{"data":{"account":{"availableBalance":42}}}`)
		default:
			return reply(503, `{}`)
		}
	})
	sid, _, err := s.StartDevice()
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.PollDevice(sid)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := s.AddFromDevice(state)
	if a == nil || err == nil {
		t.Fatal("expected partial onboarding")
	}
	if err = s.DeleteContext(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	if a, _, err = s.AddFromDevice(state); a != nil || err == nil || len(s.St.Data.Accounts) != 0 {
		t.Fatal("device retry resurrected deleted account")
	}
}
func TestUpdateRejectsWrongIdentityAndSynchronizesKeys(t *testing.T) {
	s := newService(t, func(*http.Request) (*http.Response, error) { return reply(200, `{"data":{"user_id":"other"}}`) })
	a := seed(s, "original")
	s.St.Data.Keys = []*model.Key{{ID: "owned", AccountID: a.ID, Key: "k", Enabled: true}, {ID: "legacy", Account: &model.Identity{ID: a.UserID}, Enabled: true}, {ID: "other", AccountID: "another", Enabled: true}}
	cookie := "wrong"
	if err := s.UpdateContext(context.Background(), a.ID, nil, &cookie); err == nil || a.Cookie != "original" {
		t.Fatal("wrong Cookie identity persisted")
	}
	enabled := false
	if err := s.UpdateContext(context.Background(), a.ID, &enabled, nil); err != nil {
		t.Fatal(err)
	}
	if a.Enabled || s.St.Data.Keys[0].Enabled || s.St.Data.Keys[1].Enabled || !s.St.Data.Keys[2].Enabled {
		t.Fatal("enabled state not propagated correctly")
	}
}
