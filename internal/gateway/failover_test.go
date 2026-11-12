package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

const balanceBody = `{"error":{"type":"permission_error","message":"积分余额不足，请充值后继续使用","code":"QUOTA_EXCEEDED"}}`

type failoverFixture struct {
	h    *Handler
	keys map[string]*model.Key
	hits map[string]int
	mu   sync.Mutex
}

func (f *failoverFixture) record(name string) {
	f.mu.Lock()
	f.hits[name]++
	f.mu.Unlock()
}

func (f *failoverFixture) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[name]
}

// newFailoverFixture builds a pool where the upstream dispatches on the account
// key, so a test can make one account unusable and leave the others healthy.
func newFailoverFixture(t *testing.T, names []string, upstreamFn func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string)) *failoverFixture {
	t.Helper()
	f := &failoverFixture{hits: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		name := strings.TrimPrefix(token, "key-")
		f.record(name)
		upstreamFn(f, w, r, name)
	}))
	t.Cleanup(srv.Close)

	st, _, err := store.Load(t.TempDir(), "", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.Data.Keys = nil
	f.keys = map[string]*model.Key{}
	for _, name := range names {
		k := store.NewKey(name, "key-"+name, 1)
		st.Data.Keys = append(st.Data.Keys, k)
		f.keys[name] = k
	}
	up := upstream.New(srv.URL + "/v1")
	f.h = &Handler{St: st, Picker: &pool.Picker{St: st, Up: up}, Up: up}
	return f
}

func chatPost(h *Handler, path, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+auth)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A request whose chosen account has no balance must move to a healthy account
// instead of failing the client call.
func TestBalanceFailureFailsOverToAnotherAccount(t *testing.T) {
	f := newFailoverFixture(t, []string{"poor", "rich"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		if name == "poor" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			io.WriteString(w, balanceBody)
			return
		}
		completion(w, "ok")
	})
	w := chatPost(f.h, "/v1/chat/completions", chatRequest, "x")
	if w.Code != 200 {
		t.Fatalf("client request failed instead of failing over: %d %s", w.Code, w.Body.String())
	}
	if f.count("poor") != 1 || f.count("rich") != 1 {
		t.Fatalf("unexpected upstream attempts poor=%d rich=%d", f.count("poor"), f.count("rich"))
	}
	poor, rich := f.keys["poor"], f.keys["rich"]
	now := time.Now().UnixMilli()
	if poor.Stats.Fail != 1 || poor.Stats.OK != 0 || poor.CooldownUntil < now+int64(balanceParkCooldown/time.Millisecond)-1000 {
		t.Fatalf("exhausted account not parked: %+v", poor)
	}
	if poor.Health.State != "error" || !strings.Contains(poor.Health.Error, "余额") {
		t.Fatalf("exhausted reason missing: %+v", poor.Health)
	}
	if rich.Stats.OK != 1 || rich.CooldownUntil != 0 {
		t.Fatalf("healthy account not credited: %+v", rich)
	}
	// One client request is counted once even though two upstream calls happened.
	if s := f.h.St.Data.ClientKeys[0].Stats; s.Req != 1 || s.OK != 1 || s.Fail != 0 {
		t.Fatalf("client stats double counted: %+v", s)
	}
	if len(f.h.St.Data.Logs) != 1 || f.h.St.Data.Logs[0].Status != 200 {
		t.Fatalf("expected one successful log row: %+v", f.h.St.Data.Logs)
	}
}

func TestAllAccountsExhaustedReportsAndParksEveryKey(t *testing.T) {
	f := newFailoverFixture(t, []string{"a", "b"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		io.WriteString(w, balanceBody)
	})
	w := chatPost(f.h, "/v1/chat/completions", chatRequest, "x")
	if w.Code != 403 || !strings.Contains(w.Body.String(), "积分余额不足") {
		t.Fatalf("unexpected response: %d %s", w.Code, w.Body.String())
	}
	if f.count("a") != 1 || f.count("b") != 1 {
		t.Fatalf("attempts a=%d b=%d", f.count("a"), f.count("b"))
	}
	for _, name := range []string{"a", "b"} {
		if f.keys[name].CooldownUntil <= time.Now().UnixMilli() {
			t.Fatalf("%s not parked", name)
		}
	}
	if s := f.h.St.Data.ClientKeys[0].Stats; s.Req != 1 || s.Fail != 1 || s.OK != 0 {
		t.Fatalf("client stats: %+v", s)
	}
	// With every account parked the next request is rejected locally, without
	// another billed upstream call.
	w = chatPost(f.h, "/v1/chat/completions", chatRequest, "x")
	if w.Code != 503 || f.count("a")+f.count("b") != 2 {
		t.Fatalf("parked accounts still called: %d %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if json.Unmarshal(w.Body.Bytes(), &body) != nil || !strings.Contains(w.Body.String(), "no_available_key") {
		t.Fatalf("missing machine-readable error: %s", w.Body.String())
	}
}

func TestRequestLevelErrorsAreNotRetried(t *testing.T) {
	f := newFailoverFixture(t, []string{"a", "b"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"bad request","type":"invalid_request_error","code":"bad_param"}}`)
	})
	w := chatPost(f.h, "/v1/chat/completions", chatRequest, "x")
	if w.Code != 400 || f.count("a") != 1 || f.count("b") != 0 {
		t.Fatalf("client error retried: %d hits=%d/%d", w.Code, f.count("a"), f.count("b"))
	}
	if f.keys["a"].CooldownUntil != 0 || f.keys["a"].Stats.Fail != 1 {
		t.Fatalf("client error cooled the key: %+v", f.keys["a"])
	}
}

func TestRateLimitAndServerErrorsFailOver(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"rate limit", 429, `{"error":{"message":"slow down","code":"rate_limit_exceeded"}}`},
		{"unavailable", 503, `{"error":{"message":"upstream busy"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFailoverFixture(t, []string{"first", "second"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
				if name == "first" {
					w.WriteHeader(tc.status)
					io.WriteString(w, tc.body)
					return
				}
				completion(w, "ok")
			})
			if w := chatPost(f.h, "/v1/chat/completions", chatRequest, "x"); w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			first := f.keys["first"]
			if first.CooldownUntil <= time.Now().UnixMilli() || first.Health.State == "error" && tc.status == 429 {
				t.Fatalf("expected temporary cooldown only: %+v", first)
			}
			if strings.Contains(first.Health.Error, "余额") {
				t.Fatalf("transient failure parked as billing: %+v", first.Health)
			}
			if f.keys["second"].Stats.OK != 1 {
				t.Fatalf("fallback not credited: %+v", f.keys["second"])
			}
		})
	}
}

// A streaming request may retry only while nothing has been written downstream.
func TestStreamFailsOverBeforeHeaders(t *testing.T) {
	f := newFailoverFixture(t, []string{"poor", "rich"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		if name == "poor" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(403)
			io.WriteString(w, balanceBody)
			return
		}
		streamFixture(w, false)
	})
	w := chatPost(f.h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`, "x")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "data: [DONE]") || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("stream did not fail over cleanly: %d %s", w.Code, w.Body.String())
	}
	if f.count("poor") != 1 || f.count("rich") != 1 {
		t.Fatalf("attempts poor=%d rich=%d", f.count("poor"), f.count("rich"))
	}
	if f.keys["poor"].CooldownUntil <= time.Now().UnixMilli() {
		t.Fatal("exhausted account not parked after stream failure")
	}
}

func TestStreamIsNotRetriedAfterPartialOutput(t *testing.T) {
	f := newFailoverFixture(t, []string{"flaky", "spare"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		w.Header().Set("Content-Type", "text/event-stream")
		// A partial answer with no terminal finish_reason, then a broken pipe.
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
	})
	w := chatPost(f.h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`, "x")
	if !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("truncated stream not reported: %s", w.Body.String())
	}
	if f.count("flaky") != 1 || f.count("spare") != 0 {
		t.Fatalf("retried after bytes were sent: flaky=%d spare=%d", f.count("flaky"), f.count("spare"))
	}
}

func TestBYOKIsNeverFailedOver(t *testing.T) {
	f := newFailoverFixture(t, []string{"pool-key"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
		io.WriteString(w, balanceBody)
	})
	w := chatPost(f.h, "/v1/chat/completions", chatRequest, "meoo_project_caller_owned_key")
	if w.Code != 403 {
		t.Fatalf("unexpected status %d %s", w.Code, w.Body.String())
	}
	if f.count("pool-key") != 0 {
		t.Fatal("caller-owned credential must not consume pooled accounts")
	}
	if f.h.St.Data.ClientKeys[0].Stats.Req != 0 {
		t.Fatal("BYOK request counted against a gateway API key")
	}
}

func TestLegacyAndResponsesFailOver(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/completions", `{"prompt":"hi"}`},
		{"/v1/responses", `{"input":"hi"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			f := newFailoverFixture(t, []string{"poor", "rich"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
				if name == "poor" {
					w.WriteHeader(403)
					io.WriteString(w, balanceBody)
					return
				}
				completion(w, "ok")
			})
			if w := chatPost(f.h, tc.path, tc.body, "x"); w.Code != 200 {
				t.Fatalf("%d %s", w.Code, w.Body.String())
			}
			if f.count("poor") != 1 || f.count("rich") != 1 {
				t.Fatalf("attempts poor=%d rich=%d", f.count("poor"), f.count("rich"))
			}
		})
	}
}

// Failover stops as soon as a healthy account answers, and never retries a key
// that was already attempted or parked (bad2 stays unused in this rotation).
func TestFailoverIsBoundedAndSkipsTriedKeys(t *testing.T) {
	f := newFailoverFixture(t, []string{"bad1", "bad2", "good"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		if strings.HasPrefix(name, "bad") {
			w.WriteHeader(403)
			io.WriteString(w, balanceBody)
			return
		}
		completion(w, "ok")
	})
	if w := chatPost(f.h, "/v1/chat/completions", chatRequest, "x"); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if f.count("bad1") != 1 || f.count("good") != 1 || f.count("bad2") != 0 {
		t.Fatalf("attempts bad1=%d bad2=%d good=%d", f.count("bad1"), f.count("bad2"), f.count("good"))
	}
	// The parked account must not be selected again for the same client request.
	if got := f.h.Picker.Pick(); got != nil && got.ID == f.keys["bad1"].ID {
		t.Fatal("parked key selected again")
	}
}

func TestParkedKeyIsSkippedByPicker(t *testing.T) {
	f := newFailoverFixture(t, []string{"a", "b"}, func(f *failoverFixture, w http.ResponseWriter, r *http.Request, name string) {
		completion(w, "ok")
	})
	f.h.Picker.ParkKey(f.keys["a"], balanceParkCooldown, strings.Repeat("x", 400))
	if got := f.h.Picker.Pick(); got == nil || got.ID != f.keys["b"].ID {
		t.Fatalf("parked key still selected: %+v", got)
	}
	if len(f.keys["a"].Health.Error) > 200 {
		t.Fatalf("parked reason not truncated: %d", len(f.keys["a"].Health.Error))
	}
}
