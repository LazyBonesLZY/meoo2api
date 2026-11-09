package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"meoo2api/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, _, err := Load(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestLoadCorruptPreservesOriginal(t *testing.T) {
	for _, raw := range []string{"{broken", "null", "", "[]", `{}`, `{"config":null}`, `{"version":99,"config":{}}`} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.json")
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if st, _, err := Load(dir, "", nil); err == nil {
				st.Close()
				t.Fatal("expected load error")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != raw {
				t.Fatalf("corrupt state overwritten: %q %v", got, err)
			}
		})
	}
}

func TestSingleWriterLockAndReopen(t *testing.T) {
	st := testStore(t)
	if other, _, err := Load(st.Dir, "", nil); err == nil {
		other.Close()
		t.Fatal("second writer obtained lease")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _, err := Load(st.Dir, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
}

func TestConcurrentSaveAtomic(t *testing.T) {
	st := testStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				st.Mu.Lock()
				st.Data.LastAutoRun++
				st.Mu.Unlock()
				if err := st.Save(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if err := st.Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(st.Dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var d model.State
	if err := json.Unmarshal(raw, &d); err != nil {
		t.Fatal(err)
	}
	if d.LastAutoRun != 120 {
		t.Fatalf("lost state: %d", d.LastAutoRun)
	}
	info, err := os.Stat(filepath.Join(st.Dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v", info.Mode())
	}
	matches, _ := filepath.Glob(filepath.Join(st.Dir, ".state-*.tmp"))
	if len(matches) != 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func TestMigrationLinksAreIdempotent(t *testing.T) {
	st := &Store{Data: defaults()}
	st.Data.Accounts = []*model.Account{{ID: "a1", UserID: "u1", Enabled: true}, nil}
	st.Data.Checkins = []*model.Checkin{{ID: "legacy", Cookie: "cookie", Account: &model.Identity{ID: "u2", Name: "two"}, Enabled: true, PingOK: true}}
	st.Data.Keys = []*model.Key{{ID: "k1", Key: "key", Label: "project", Enabled: true, Account: &model.Identity{ID: "u1"}}, nil}
	st.Data.PersonalKeys = []*model.PersonalKey{{ID: "pk1", AccountID: "u1"}}
	st.migrate()
	st.migrate()
	if len(st.Data.Accounts) != 2 || st.Data.Accounts[0].KeyID != "k1" || st.Data.Keys[0].AccountID != "a1" {
		t.Fatalf("migration failed: %+v", st.Data)
	}
	if len(st.Data.Accounts[0].ProjectKeyIDs) != 1 || len(st.Data.Accounts[0].PersonalKeyIDs) != 1 {
		t.Fatal("duplicate links")
	}
	if st.Data.Accounts[0].ProjectID != "project" {
		t.Fatal("missing project identity")
	}
	if !st.Data.Accounts[1].PingOK {
		t.Fatal("lost legacy session state")
	}
}

func TestRecordAndAccountDisable(t *testing.T) {
	st := testStore(t)
	st.Data.Config.MaxLogs = 2
	st.Data.Config.MaxUsageHours = 1
	st.Data.Accounts = []*model.Account{{ID: "a", Enabled: true, Expired: true}}
	key := &model.Key{ID: "k", AccountID: "a", Enabled: true, Key: "project-key"}
	st.Data.Keys = []*model.Key{key}
	if !st.UsableKeyLocked(key, time.Now().UnixMilli()) {
		t.Fatal("cookie expiry incorrectly disables AK")
	}
	st.Data.Accounts[0].Enabled = false
	if st.UsableKeyLocked(key, time.Now().UnixMilli()) {
		t.Fatal("disabled account remains usable")
	}
	st.Data.Usage["2000-01-01T00"] = &model.Bucket{}
	for i := 0; i < 3; i++ {
		st.Record(&model.LogRow{Status: 200, Model: "m", PT: 1, CT: 2, Ms: 10})
	}
	if len(st.Data.Logs) != 2 || len(st.Data.Usage) != 1 {
		t.Fatal("retention failed")
	}
	for _, b := range st.Data.Usage {
		if b.Req != 3 || b.PT != 3 || b.CT != 6 {
			t.Fatalf("incorrect usage: %+v", b)
		}
	}
}

func TestRecordCopiesAndRedacts(t *testing.T) {
	st := testStore(t)
	st.Data.Keys = []*model.Key{{Key: "meoo_project_private_value"}}
	row := &model.LogRow{Status: 502, Error: "upstream echoed meoo_project_private_value", Rid: "original"}
	st.Record(row)
	row.Error = "mutated"
	row.Rid = "changed"
	if st.Data.Logs[0].Rid != "original" || st.Data.Logs[0].Error != "upstream echoed [redacted]" {
		t.Fatalf("unsafe stored log: %+v", st.Data.Logs[0])
	}
}

func TestLoadBootstrapAndPassword(t *testing.T) {
	t.Setenv("MEOO2API_ADMIN_PASSWORD", "example-password-123")
	st, generated, err := Load(t.TempDir(), "meoo_project_example", []string{"sk-example"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if generated != "" || !model.VerifyPassword(st.Data.Config.AdminPass, "example-password-123") {
		t.Fatal("bootstrap password failed")
	}
	if len(st.Data.Keys) != 1 || len(st.Data.ClientKeys) != 1 || st.Data.Accounts == nil {
		t.Fatal("bootstrap state incomplete")
	}
}
