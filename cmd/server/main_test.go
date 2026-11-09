package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/admin"
	"meoo2api/internal/gateway"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	st, _, err := store.Load(t.TempDir(), "", []string{"sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	up := upstream.New("http://127.0.0.1:1/v1")
	picker := &pool.Picker{St: st, Up: up}
	adm := &admin.Handler{St: st, Picker: picker, Acc: &account.Service{St: st, Up: up}, Tokens: admin.NewTokens(), Start: time.Now()}
	return newHandler(st, picker, &gateway.Handler{St: st, Picker: picker, Up: up}, adm, up.Base)
}

func TestRoutesAndHeaders(t *testing.T) {
	h := testHandler(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"HEAD", "/", 200}, {"POST", "/", 405},
		{"GET", "/healthz", 200}, {"POST", "/healthz", 405},
		{"GET", "/not-a-route", 404}, {"GET", "/admin/api/state", 401},
		{"OPTIONS", "/v1/chat/completions", 204},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
		if tc.path == "/" && tc.method == "GET" && (w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("X-Frame-Options") != "DENY") {
			t.Error("missing UI headers")
		}
		if strings.HasPrefix(tc.path, "/admin/") && w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("admin API permits wildcard origin")
		}
		if tc.method == "HEAD" && w.Body.Len() != 0 {
			t.Error("HEAD returned a body")
		}
	}
}

func TestPanicProducesSingleJSONError(t *testing.T) {
	h := newHandler(nil, nil, nil, nil, "")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
	if w.Code != 500 {
		t.Fatalf("panic status: %d", w.Code)
	}
	var d map[string]any
	if json.Unmarshal(w.Body.Bytes(), &d) != nil || d["error"] == nil {
		t.Fatalf("invalid panic response %s", w.Body.String())
	}
}

func TestBodyLimitAppliesToGateway(t *testing.T) {
	h := testHandler(t)
	payload := `{"model":"qwen","messages":[{"role":"user","content":"` + strings.Repeat("a", maxBodyBytes) + `"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(payload))
	r.Header.Set("Authorization", "Bearer sk-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversize body status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestStateWriterFlushAndDuplicateHeader(t *testing.T) {
	raw := httptest.NewRecorder()
	w := &stateWriter{ResponseWriter: raw}
	w.Flush()
	w.WriteHeader(500)
	_, _ = w.Write([]byte("data"))
	if !w.wrote || raw.Code != 200 || raw.Body.String() != "data" || !raw.Flushed {
		t.Fatal("response state tracking incorrect")
	}
	if w.Unwrap() != raw {
		t.Fatal("response controller cannot unwrap")
	}
}
