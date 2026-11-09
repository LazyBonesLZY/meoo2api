package checkin

import (
	"context"
	"io"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(code int, body string) (*http.Response, error) {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
}
func testStore(t *testing.T) (*store.Store, *model.Account) {
	t.Helper()
	st, _, err := store.Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := any(st).(interface{ Close() error }); ok {
		t.Cleanup(func() { _ = closer.Close() })
	}
	a := &model.Account{ID: "a", Label: "mock", UserID: "user", Cookie: "session=mock", Enabled: true}
	st.Data.Accounts = []*model.Account{a}
	return st, a
}
func TestConcurrentCheckinSingleRequestAndDailyAccounting(t *testing.T) {
	st, a := testStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Cookie") != "session=mock" {
			t.Error("missing Cookie")
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return reply(200, `{"success":true,"data":{"initialized":true,"dailyLoginGranted":true,"dailyLoginGrantAmount":10,"dailyLoginBalanceAfter":99}}`)
	})}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if last := r.RunContext(context.Background(), st, a); !last.OK {
				t.Errorf("checkin: %s", last.Error)
			}
		}()
	}
	<-entered
	deadline := time.Now().Add(time.Second)
	for {
		gates.Lock()
		g := gates.values[gateKey{st, a}]
		refs := g.refs
		gates.Unlock()
		if refs == 8 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			wg.Wait()
			t.Fatal("concurrent checkins did not enter gate")
		}
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("sent %d concurrent requests", calls.Load())
	}
	// A subsequent explicit retry may call upstream but must not double-count grants.
	last := r.RunContext(context.Background(), st, a)
	if !last.OK || a.GrantCount != 1 || a.TotalGranted != 10 || a.Points != 99 {
		t.Fatalf("double accounting: %+v", a)
	}
	gates.Lock()
	n := len(gates.values)
	gates.Unlock()
	if n != 0 {
		t.Fatal("account gate leaked")
	}
}
func TestCheckinFailuresAndCredentialReplacement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int
		body    string
		expired bool
	}{{"auth", 401, `{}`, true}, {"server", 503, `{}`, false}, {"malformed", 200, `{`, false}, {"declined", 200, `{"success":false}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			st, a := testStore(t)
			r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return reply(tc.code, tc.body) })}}
			last := r.RunContext(context.Background(), st, a)
			if last.OK || last.Error == "" || a.Expired != tc.expired {
				t.Fatalf("last=%+v expired=%v", last, a.Expired)
			}
		})
	}
	st, a := testStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { close(entered); <-release; return reply(401, `{}`) })}}
	done := make(chan struct{})
	go func() { defer close(done); r.RunContext(context.Background(), st, a) }()
	<-entered
	st.Mu.Lock()
	a.Cookie = "replacement"
	st.Mu.Unlock()
	close(release)
	<-done
	if a.Expired || a.Last != nil {
		t.Fatal("stale checkin poisoned replacement Cookie")
	}
}
func TestMissingCookiePersistsLastAndDeviceOnlyIsSkipped(t *testing.T) {
	st, a := testStore(t)
	a.Cookie = ""
	last := Run(st, a)
	if last.Error == "" || a.Last == nil {
		t.Fatal("missing Cookie error was not saved")
	}
	if results := RunAll(st, true); len(results) != 0 {
		t.Fatal("device-only account sent to Cookie-only checkin")
	}
}
func TestCancelledCheckinDoesNotPoisonCredential(t *testing.T) {
	st, a := testStore(t)
	entered := make(chan struct{})
	r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(entered)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan model.CheckinLast, 1)
	go func() { done <- r.RunContext(ctx, st, a) }()
	<-entered
	cancel()
	last := <-done
	if last.OK || last.Error == "" || a.Expired {
		t.Fatal("cancel incorrectly expired credential")
	}
}
func TestSchedulerRechecksStateAfterFailedPing(t *testing.T) {
	st, a := testStore(t)
	st.Data.Config.AutoCheckin = true
	st.Data.Config.CheckinHour = 0
	st.Data.Config.KeepAliveHours = 1
	var pings atomic.Int32
	runContext(context.Background(), st, func(ctx context.Context, x *model.Account) (bool, error) {
		pings.Add(1)
		st.Mu.Lock()
		x.Expired = true
		x.LastPing = time.Now().UnixMilli()
		st.Mu.Unlock()
		return false, nil
	})
	if pings.Load() != 1 || a.Last != nil {
		t.Fatal("scheduler ran checkin after failed keepalive")
	}
	// Expired accounts still get a future keepalive attempt and can recover.
	st.Mu.Lock()
	a.LastPing = 0
	a.Cookie = ""
	st.Mu.Unlock()
	runContext(context.Background(), st, func(ctx context.Context, x *model.Account) (bool, error) {
		pings.Add(1)
		st.Mu.Lock()
		x.Expired = false
		x.LastPing = time.Now().UnixMilli()
		st.Mu.Unlock()
		return true, nil
	})
	if pings.Load() != 2 || a.Expired || a.Last != nil {
		t.Fatal("expired/device-only account scheduler behavior wrong")
	}
}
func TestSchedulerStopsTickerAndInflightPing(t *testing.T) {
	st, _ := testStore(t)
	st.Data.Config.KeepAliveHours = 1
	entered := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	done := startScheduler(ctx, st, func(ctx context.Context, a *model.Account) (bool, error) {
		close(entered)
		<-ctx.Done()
		return false, ctx.Err()
	}, time.Millisecond)
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler failed to stop in-flight ping")
	}
	st.Mu.RLock()
	at := st.Data.LastAutoRun
	st.Mu.RUnlock()
	if at != 0 {
		t.Fatal("cancelled scheduler recorded a completed run")
	}
	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	var calls atomic.Int32
	done = startScheduler(cancelled, st, func(context.Context, *model.Account) (bool, error) { calls.Add(1); return true, nil }, time.Millisecond)
	<-done
	if calls.Load() != 0 {
		t.Fatal("cancelled scheduler performed work")
	}
}
func TestCheckinDoesNotFollowCredentialRedirect(t *testing.T) {
	st, a := testStore(t)
	var calls atomic.Int32
	r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		resp, _ := reply(302, `{}`)
		resp.Header.Set("Location", "https://meoo.com/redirected")
		return resp, nil
	})}}
	if last := r.RunContext(context.Background(), st, a); last.OK || calls.Load() != 1 {
		t.Fatal("checkin followed credential redirect")
	}
}
func TestRemovedAccountNotUpdatedByInflightCheckin(t *testing.T) {
	st, a := testStore(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r := &Runner{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return reply(200, `{"success":true,"data":{"dailyLoginGranted":true,"dailyLoginGrantAmount":5}}`)
	})}}
	done := make(chan struct{})
	go func() { defer close(done); r.RunContext(context.Background(), st, a) }()
	<-entered
	st.Mu.Lock()
	st.Data.Accounts = nil
	st.Mu.Unlock()
	close(release)
	<-done
	if a.Last != nil || a.GrantCount != 0 {
		t.Fatal("removed account updated by stale response")
	}
}
