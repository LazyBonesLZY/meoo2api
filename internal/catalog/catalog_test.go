package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type clock struct{ nanoseconds atomic.Int64 }

func newClock() *clock                   { c := &clock{}; c.nanoseconds.Store(time.Now().UnixNano()); return c }
func (c *clock) now() time.Time          { return time.Unix(0, c.nanoseconds.Load()) }
func (c *clock) advance(d time.Duration) { c.nanoseconds.Add(int64(d)) }

func fixture(t *testing.T, handler http.HandlerFunc) (*Service, *clock) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	st, _, err := store.Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	st.Data.Keys = []*model.Key{store.NewKey("fixture", "pool-key", 1)}
	s := New(st, upstream.New(srv.URL+"/v1/"))
	c := newClock()
	s.timeNow = c.now
	return s, c
}
func native(w http.ResponseWriter, ids ...string) {
	w.Header().Set("Content-Type", "application/json")
	if ids == nil {
		ids = []string{}
	}
	json.NewEncoder(w).Encode(map[string]any{"models": ids, "defaultModel": "a"})
}
func ids(s Snapshot) string {
	out := []string{}
	for _, entry := range s.Data {
		out = append(out, entry.ID)
	}
	return strings.Join(out, ",")
}

func TestSyncFullReplacementAdditionDeletionEmpty(t *testing.T) {
	var version, hits atomic.Int32
	s, c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer pool-key" || r.Header.Get("Accept") != "application/json" {
			t.Error("bad model directory request", r.Method, r.URL, r.Header)
		}
		switch version.Load() {
		case 0:
			native(w, "a", "b", "a")
		case 1:
			native(w, "a", "b", "c")
		case 2:
			native(w, "c")
		case 3:
			native(w)
		}
	})
	for i, want := range []string{"a,b", "a,b,c", "c", ""} {
		version.Store(int32(i))
		if i > 0 {
			c.advance(cacheTTL)
		}
		got := s.Get(context.Background(), "")
		if ids(got) != want || got.Source != "upstream" || got.Stale || got.Error != "" || got.UpdatedAt != c.now().UnixMilli() {
			t.Fatalf("sync %d=%+v want %q", i, got, want)
		}
		if i < 2 && got.DefaultModel != "a" {
			t.Fatalf("default lost: %+v", got)
		}
		if i >= 2 && got.DefaultModel != "" {
			t.Fatal("nonexistent default retained", got)
		}
		if i == 3 && got.Data == nil {
			t.Fatal("empty list must encode [] rather than null")
		}
	}
	if hits.Load() != 4 {
		t.Fatal("not refreshed", hits.Load())
	}
}

func TestCacheTTLBackoffAndStalePreservation(t *testing.T) {
	var fail atomic.Bool
	var hits atomic.Int32
	s, c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail.Load() {
			w.WriteHeader(503)
			io.WriteString(w, "pool-key secret error")
			return
		}
		native(w, "a")
	})
	first := s.Get(context.Background(), "")
	first.Data[0].ID = "mutated"
	for i := 0; i < 10; i++ {
		got := s.Get(context.Background(), "")
		if ids(got) != "a" || got.Stale {
			t.Fatal("cache copy/ttl", got)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("fresh hit upstream")
	}
	fail.Store(true)
	c.advance(cacheTTL)
	got := s.Get(context.Background(), "")
	if got.Source != "upstream" || !got.Stale || got.Error == "" || ids(got) != "a" || got.UpdatedAt != first.UpdatedAt || strings.Contains(got.Error, "pool-key") {
		t.Fatal("stale", got)
	}
	for i := 0; i < 10; i++ {
		s.Get(context.Background(), "")
	}
	c.advance(failureBackoff - time.Millisecond)
	s.Get(context.Background(), "")
	if hits.Load() != 2 {
		t.Fatal("backoff ignored", hits.Load())
	}
	fail.Store(false)
	c.advance(time.Millisecond)
	got = s.Get(context.Background(), "")
	if got.Stale || got.Error != "" || got.UpdatedAt == first.UpdatedAt || hits.Load() != 3 {
		t.Fatal("recovery", got, hits.Load())
	}
}

func TestInitialFallbackNoKeysAndFailedBackoff(t *testing.T) {
	var hits atomic.Int32
	s, c := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(401)
		fmt.Fprint(w, "pool-key")
	})
	got := s.Get(context.Background(), "")
	if got.Source != "builtin" || !got.Stale || got.Error == "" || got.DefaultModel != "qwen3.8-flash" || len(got.Data) != len(model.NativeModels) || strings.Contains(got.Error, "pool-key") {
		t.Fatal(got)
	}
	s.Get(context.Background(), "")
	if hits.Load() != 1 {
		t.Fatal("no backoff")
	}
	c.advance(failureBackoff)
	s.Get(context.Background(), "")
	if hits.Load() != 2 {
		t.Fatal("no retry")
	}
	s.st.Mu.Lock()
	s.st.Data.Keys = nil
	s.st.Mu.Unlock()
	c.advance(failureBackoff)
	got = s.Get(context.Background(), "")
	if got.Error != "no available key for model directory" || hits.Load() != 2 {
		t.Fatal(got, hits.Load())
	}
	got.Data[0].ID = "changed"
	if Builtin().Data[0].ID == "changed" {
		t.Fatal("builtin shared")
	}
}

func TestConcurrentRefreshCoalesces(t *testing.T) {
	var hits atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
		native(w, "a")
	})
	var wg sync.WaitGroup
	results := make(chan Snapshot, 40)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); results <- s.Get(context.Background(), "") }()
	}
	<-started
	close(release)
	wg.Wait()
	close(results)
	for got := range results {
		if ids(got) != "a" || got.Stale {
			t.Fatal(got)
		}
	}
	if hits.Load() != 1 {
		t.Fatal("concurrent refresh duplicated", hits.Load())
	}
}

func TestPoolFailoverAtMostThreeUniqueUsableKeys(t *testing.T) {
	var mu sync.Mutex
	seen := []string{}
	var success atomic.Bool
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen = append(seen, tok)
		mu.Unlock()
		if tok == "key-3" && success.Load() {
			native(w, "live")
			return
		}
		w.WriteHeader(401)
	})
	keys := []*model.Key{}
	for i := 1; i <= 4; i++ {
		keys = append(keys, store.NewKey("fixture", fmt.Sprintf("key-%d", i), 1))
	}
	duplicate := store.NewKey("dup", "key-1", 1)
	disabled := store.NewKey("disabled", "disabled", 1)
	disabled.Enabled = false
	cooling := store.NewKey("cooling", "cooling", 1)
	cooling.CooldownUntil = time.Now().Add(time.Hour).UnixMilli()
	account := store.NewKey("account", "account", 1)
	account.AccountID = "disabled-account"
	s.st.Data.Accounts = []*model.Account{{ID: "disabled-account", Enabled: false}}
	s.st.Data.Keys = append([]*model.Key{disabled, cooling, account, keys[0], duplicate}, keys[1:]...)
	got := s.Get(context.Background(), "")
	if got.Source != "builtin" || strings.Join(seen, ",") != "key-1,key-2,key-3" {
		t.Fatal("failover", got, seen)
	}
	for _, k := range keys {
		if k.Stats.Req != 0 || k.Health.State != "unknown" || k.CooldownUntil != 0 {
			t.Fatal("catalog changed key health", k)
		}
	}
	success.Store(true)
	s.mu.Lock()
	s.pool.retryAfter = time.Time{}
	s.mu.Unlock()
	got = s.Get(context.Background(), "")
	if ids(got) != "live" || got.Source != "upstream" || got.Stale {
		t.Fatal("third key not used", got)
	}
}

func TestBYOKIsolationBoundedLRUAndCopies(t *testing.T) {
	var hits atomic.Int32
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		native(w, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	})
	pool := s.Get(context.Background(), "")
	a := s.Get(context.Background(), "byok-a")
	b := s.Get(context.Background(), "byok-b")
	if ids(pool) != "pool-key" || ids(a) != "byok-a" || ids(b) != "byok-b" {
		t.Fatal(pool, a, b)
	}
	a.Data[0].ID = "changed"
	if ids(s.Get(context.Background(), "byok-a")) != "byok-a" || ids(s.Get(context.Background(), "")) != "pool-key" || hits.Load() != 3 {
		t.Fatal("cache polluted")
	}
	for i := 0; i < maxBYOKCaches-2; i++ {
		s.Get(context.Background(), fmt.Sprintf("byok-%d", i))
	}
	if len(s.byok) != maxBYOKCaches {
		t.Fatal("capacity", len(s.byok))
	}
	s.Get(context.Background(), "byok-a") // touch a, making b the least recently used
	s.Get(context.Background(), "extra")
	if len(s.byok) != maxBYOKCaches || s.byok[sha256.Sum256([]byte("byok-b"))] != nil || s.byok[sha256.Sum256([]byte("byok-a"))] == nil {
		t.Fatal("LRU eviction wrong")
	}
	if s.st.Data.Keys[0].Stats.Req != 0 {
		t.Fatal("model listing counted completion")
	}
}

func TestCapacityNeverEvictsInflight(t *testing.T) {
	var hits atomic.Int32
	allStarted, release := make(chan struct{}), make(chan struct{})
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == maxBYOKCaches {
			close(allStarted)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		native(w, "a")
	})
	var wg sync.WaitGroup
	for i := 0; i < maxBYOKCaches; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); s.Get(context.Background(), fmt.Sprint(i)) }(i)
	}
	<-allStarted
	got := s.Get(context.Background(), "overflow")
	if got.Source != "builtin" || got.Error != "model directory cache is busy" || len(s.byok) != maxBYOKCaches || hits.Load() != maxBYOKCaches {
		t.Fatal(got)
	}
	close(release)
	wg.Wait()
}

func TestCancellationWaiterOwnerAndStart(t *testing.T) {
	t.Run("waiter", func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			native(w, "a")
		})
		owner := make(chan Snapshot, 1)
		go func() { owner <- s.Get(context.Background(), "") }()
		<-started
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		got := s.Get(ctx, "")
		if !got.Stale || got.Error != "model directory request cancelled" {
			t.Fatal(got)
		}
		close(release)
		if got := <-owner; got.Stale || ids(got) != "a" {
			t.Fatal("waiter cancelled owner", got)
		}
	})
	t.Run("start owner", func(t *testing.T) {
		started := make(chan struct{})
		s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			close(started)
			select {
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
		})
		ctx, cancel := context.WithCancel(context.Background())
		done := s.Start(ctx)
		<-started
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Start did not stop")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.pool.retryAfter.IsZero() || s.pool.snapshot.Error != "" || s.pool.inflight != nil {
			t.Fatal("cancellation poisoned cache", s.pool)
		}
	})
	t.Run("start immediate and periodic", func(t *testing.T) {
		var hits atomic.Int32
		seen := make(chan struct{}, 10)
		s, c := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); native(w, "a"); seen <- struct{}{} })
		s.refreshInterval = 10 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := s.Start(ctx)
		select {
		case <-seen:
		case <-time.After(time.Second):
			t.Fatal("no initial refresh")
		}
		// Let Get install its result before advancing the injected clock.
		s.Get(context.Background(), "")
		c.advance(cacheTTL)
		select {
		case <-seen:
		case <-time.After(time.Second):
			t.Fatal("no periodic refresh")
		}
		cancel()
		<-done
		if hits.Load() < 2 {
			t.Fatal("no periodic synchronization")
		}
	})
}

func TestNoRedirectAndNoErrorSecretLeak(t *testing.T) {
	var destHits atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destHits.Add(1); native(w, "redirect") }))
	defer dest.Close()
	s, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	})
	s.up.HTTP = &http.Client{} // even supplied clients must not follow redirects
	key := "byok-secret-token"
	got := s.Get(context.Background(), key)
	if destHits.Load() != 0 || !got.Stale || got.Source != "builtin" || strings.Contains(got.Error, key) || !strings.Contains(got.Error, "307") {
		t.Fatal(got, destHits.Load())
	}
	s.up.Base = ":bad-url-" + key
	got = s.Get(context.Background(), "another-key")
	if strings.Contains(got.Error, key) || got.Error != "invalid upstream model directory URL" {
		t.Fatal(got)
	}
}

func TestReadFailuresAndResponseLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"limit", func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, strings.Repeat("x", maxResponseBytes+1))
		}},
		{"truncated", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, `{"models":["a"]}`)
		}},
		{"malformed", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `not-json`) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := fixture(t, tc.handler)
			got := s.Get(context.Background(), "secret")
			if got.Error == "" || got.Source != "builtin" || !got.Stale {
				t.Fatal(got)
			}
		})
	}
}

func TestParseNativeOpenAIAndStrictValidation(t *testing.T) {
	list, def, err := parse([]byte(`{"data":[{"id":"model-a","object":"model","created":123,"owned_by":"provider"},{"id":"model-a","object":"model","created":123,"owned_by":"provider"}],"defaultModel":"model-a"}`))
	if err != nil || len(list) != 1 || list[0].Created != 123 || list[0].OwnedBy != "provider" || def != "model-a" {
		t.Fatal(list, def, err)
	}
	for _, valid := range []string{`{"models":[]}`, `{"data":[]}`, `{"models":["a"],"defaultModel":"missing"}`} {
		if _, _, err := parse([]byte(valid)); err != nil {
			t.Fatal(valid, err)
		}
	}
	for _, invalid := range []string{
		`null`, `[]`, `{}`, `{"models":null}`, `{"models":[null]}`, `{"models":[1]}`, `{"models":[""]}`, `{"models":[" a"]}`, `{"models":["a\n"]}`, `{"models":[],"defaultModel":1}`, `{"models":[],"defaultModel":null}`, `{"models":["a"],"success":false}`, `{"models":["a"],"success":null}`, `{"models":[]} {}`,
		`{"data":null}`, `{"data":[null]}`, `{"data":[{}]}`, `{"data":[{"id":"a","object":"other","created":0,"owned_by":"p"}]}`, `{"data":[{"id":"a","object":"model","created":null,"owned_by":"p"}]}`, `{"data":[{"id":"a","object":"model","created":-1,"owned_by":"p"}]}`, `{"data":[{"id":"a","object":"model","created":1.5,"owned_by":"p"}]}`, `{"data":[{"id":"a","object":"model","created":0,"owned_by":""}]}`, `{"data":[],"error":{"message":"secret"}}`,
	} {
		if _, _, err := parse([]byte(invalid)); err == nil {
			t.Errorf("accepted invalid directory: %s", invalid)
		}
	}
	fallback := Builtin()
	fallback.Data[0].ID = "mutated"
	if Builtin().Data[0].ID == "mutated" {
		t.Fatal("Builtin shared backing")
	}
}
