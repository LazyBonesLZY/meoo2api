package pool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

func testPicker(t *testing.T) *Picker {
	t.Helper()
	st, _, err := store.Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Picker{St: st, Up: upstream.New("http://fixture")}
}

func TestWeightedStickyAccountAndSnapshots(t *testing.T) {
	p := testPicker(t)
	a, b := store.NewKey("a", "a", 2), store.NewKey("b", "b", 1)
	p.St.Data.Keys = []*model.Key{a, b, nil}
	for i, want := range []string{a.ID, a.ID, b.ID, a.ID, a.ID, b.ID} {
		if got := p.Pick(); got == nil || got.ID != want {
			t.Fatalf("pick %d=%v want %s", i, got, want)
		}
	}
	snapshot := p.Pick()
	snapshot.Key = "modified"
	if a.Key != "a" {
		t.Fatal("Pick exposed shared key")
	}
	usable := p.Usable()
	usable[0].Stats.Req++
	if a.Stats.Req != 0 {
		t.Fatal("Usable exposed shared stats")
	}
	p.St.Data.Config.Sticky = true
	first := p.Pick().ID
	for i := 0; i < 10; i++ {
		if p.Pick().ID != first {
			t.Fatal("sticky moved")
		}
	}
	a.AccountID = "account"
	b.Enabled = false
	p.St.Data.Accounts = []*model.Account{{ID: "account", Enabled: false}}
	if p.Pick() != nil {
		t.Fatal("disabled account was used")
	}
	p.St.Data.Accounts[0].Enabled = true
	p.St.Data.Accounts[0].Expired = true // cookie expiry does not invalidate AK
	if p.Pick() == nil {
		t.Fatal("enabled AK unavailable")
	}
	p.St.Data.Accounts = nil
	if p.Pick() != nil {
		t.Fatal("orphaned account was used")
	}
}

func TestRecordKeyAndReplacement(t *testing.T) {
	p := testPicker(t)
	k := store.NewKey("a", "secret", 1)
	p.St.Data.Keys = []*model.Key{k}
	snapshot := p.Pick()
	p.RecordKey(snapshot, false, false, false, 0, 0)
	if k.CooldownUntil != 0 || k.Stats.Fail != 1 {
		t.Fatal("caller error cooled key")
	}
	p.MarkKey(snapshot, false, true)
	until := k.CooldownUntil
	if until <= time.Now().UnixMilli() {
		t.Fatal("no cooldown")
	}
	p.RecordKey(snapshot, true, false, false, 3, 4)
	if k.CooldownUntil != until || k.Stats.PT != 3 || k.Stats.CT != 4 {
		t.Fatal("success erased newer cooldown or usage missing")
	}
	k.Key = "replacement"
	p.MarkKey(snapshot, false, false)
	if k.Stats.Req != 3 {
		t.Fatal("stale key result updated replacement")
	}
	p.St.Data.Keys = nil
	p.MarkKey(snapshot, true, false)
}

func TestConcurrentPickerAndConfig(t *testing.T) {
	p := testPicker(t)
	p.St.Data.Keys = []*model.Key{store.NewKey("a", "a", 2), store.NewKey("b", "b", 1)}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				k := p.Pick()
				p.Usable()
				p.RecordKey(k, true, false, false, 1, 2)
				p.St.Mu.Lock()
				p.St.Data.Config.Sticky = j%2 == 0
				p.St.Data.Keys[0].Label = "changed"
				p.St.Mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var req int64
	for _, k := range p.St.Data.Keys {
		req += k.Stats.Req
	}
	if req != 1600 {
		t.Fatalf("req=%d", req)
	}
}

func TestProbeTransportAndDelete(t *testing.T) {
	p := testPicker(t)
	k := store.NewKey("a", "secret", 1)
	p.St.Data.Keys = []*model.Key{k}
	started, release := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("bad probe request")
		}
		close(started)
		<-release
		w.WriteHeader(401)
	}))
	defer srv.Close()
	p.Up = upstream.New(srv.URL + "/")
	finished := make(chan model.Health, 1)
	go func() { finished <- p.Probe(k) }()
	<-started
	p.St.Mu.Lock()
	p.St.Data.Keys = nil
	p.St.Mu.Unlock()
	close(release)
	if h := <-finished; h.State != "invalid" {
		t.Fatalf("health=%+v", h)
	}
	if k.Health.State != "unknown" {
		t.Fatal("deleted key was modified")
	}
	p.Up.Base = ":bad url"
	p.St.Data.Keys = []*model.Key{k}
	if h := p.Probe(k); h.State != "error" {
		t.Fatalf("bad URL health=%+v", h)
	}
}

func TestPerCallerStickyBoundedAndCancellation(t *testing.T) {
	p := testPicker(t)
	p.St.Data.Keys = []*model.Key{store.NewKey("a", "a", 1), store.NewKey("b", "b", 1)}
	p.St.Data.Config.Sticky = true
	a, b := p.PickFor("client-a"), p.PickFor("client-b")
	if a.ID == b.ID {
		t.Fatal("distinct callers forced into global affinity")
	}
	for i := 0; i < 20; i++ {
		if p.PickFor("client-a").ID != a.ID || p.PickFor("client-b").ID != b.ID {
			t.Fatal("caller affinity moved")
		}
	}
	for i := 0; i < 5000; i++ {
		p.PickFor(string(rune(i + 1)))
	}
	if len(p.stickyByCaller) > 4096 {
		t.Fatal("unbounded caller map")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := p.St.Data.Keys[0].Health
	p.ProbeContext(ctx, a)
	if p.St.Data.Keys[0].Health != before {
		t.Fatal("cancelled probe polluted health")
	}
}
