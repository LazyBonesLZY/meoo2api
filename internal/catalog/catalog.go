// Package catalog synchronizes upstream model directories without generating
// completions, changing key health, or persisting caller credentials.
package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

const (
	cacheTTL         = 5 * time.Minute
	failureBackoff   = 30 * time.Second
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 1 << 20
	maxBYOKCaches    = 64
)

type Entry struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type Snapshot struct {
	Data         []Entry `json:"data"`
	Source       string  `json:"source"`
	UpdatedAt    int64   `json:"updatedAt"`
	Stale        bool    `json:"stale"`
	Error        string  `json:"error"`
	DefaultModel string  `json:"defaultModel"`
}

type cacheSlot struct {
	snapshot   Snapshot
	updated    time.Time
	retryAfter time.Time
	inflight   chan struct{}
	lastAccess uint64
}

type Service struct {
	st     *store.Store
	up     *upstream.Client
	mu     sync.Mutex
	pool   cacheSlot
	byok   map[[sha256.Size]byte]*cacheSlot
	access uint64
	// Set only before starting concurrent work; injectable for deterministic tests.
	timeNow         func() time.Time
	refreshInterval time.Duration
}

func New(st *store.Store, up *upstream.Client) *Service {
	return &Service{st: st, up: up, pool: cacheSlot{snapshot: builtin()},
		byok: map[[sha256.Size]byte]*cacheSlot{}, timeNow: time.Now, refreshInterval: cacheTTL}
}

// Builtin returns an independent initial fallback for optional integrations.
func Builtin() Snapshot { return builtin() }

func builtin() Snapshot {
	entries := make([]Entry, 0, len(model.NativeModels))
	for _, id := range model.NativeModels {
		entries = append(entries, Entry{ID: id, Object: "model", OwnedBy: "meoo"})
	}
	return Snapshot{Data: entries, Source: "builtin", Stale: true, DefaultModel: "qwen3.8-flash"}
}

func clone(snapshot Snapshot) Snapshot {
	snapshot.Data = append(make([]Entry, 0, len(snapshot.Data)), snapshot.Data...)
	return snapshot
}

// slotLocked keeps only credential digests, never raw BYOK credentials. Active
// refreshes are not evicted, so concurrent calls still share a single request.
func (s *Service) slotLocked(byok string) *cacheSlot {
	if byok == "" {
		return &s.pool
	}
	id := sha256.Sum256([]byte(byok))
	if slot := s.byok[id]; slot != nil {
		return slot
	}
	if len(s.byok) >= maxBYOKCaches {
		var victim [sha256.Size]byte
		var oldest *cacheSlot
		for key, slot := range s.byok {
			if slot.inflight == nil && (oldest == nil || slot.lastAccess < oldest.lastAccess) {
				victim, oldest = key, slot
			}
		}
		if oldest == nil {
			return nil
		}
		delete(s.byok, victim)
	}
	slot := &cacheSlot{snapshot: builtin()}
	s.byok[id] = slot
	return slot
}

func (s *Service) snapshotLocked(slot *cacheSlot, now time.Time) Snapshot {
	out := clone(slot.snapshot)
	out.Stale = out.Source != "upstream" || !now.Before(slot.updated.Add(cacheTTL)) || out.Error != ""
	return out
}

// Get returns an independent directory. Expired refreshes are coalesced per
// credential scope; failure retains the last successful full directory.
func (s *Service) Get(ctx context.Context, byok string) Snapshot {
	for {
		s.mu.Lock()
		slot := s.slotLocked(byok)
		if slot == nil {
			s.mu.Unlock()
			out := builtin()
			out.Error = "model directory cache is busy"
			return out
		}
		s.access++
		slot.lastAccess = s.access
		now := s.timeNow()
		if ctx.Err() != nil {
			out := s.snapshotLocked(slot, now)
			out.Error = "model directory request cancelled"
			out.Stale = true
			s.mu.Unlock()
			return out
		}
		if slot.inflight != nil {
			done := slot.inflight
			s.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				s.mu.Lock()
				out := s.snapshotLocked(slot, s.timeNow())
				s.mu.Unlock()
				out.Error = "model directory request cancelled"
				out.Stale = true
				return out
			}
		}
		if now.Before(slot.retryAfter) || (slot.snapshot.Source == "upstream" && slot.snapshot.Error == "" && now.Before(slot.updated.Add(cacheTTL))) {
			out := s.snapshotLocked(slot, now)
			s.mu.Unlock()
			return out
		}
		done := make(chan struct{})
		slot.inflight = done
		s.mu.Unlock()

		entries, defaultModel, err := s.refresh(ctx, byok)
		s.mu.Lock()
		now = s.timeNow()
		if ctx.Err() == nil {
			if err == nil {
				slot.snapshot = Snapshot{Data: entries, Source: "upstream", UpdatedAt: now.UnixMilli(), DefaultModel: defaultModel}
				slot.updated = now
				slot.retryAfter = time.Time{}
			} else {
				slot.snapshot.Error = err.Error()
				slot.retryAfter = now.Add(failureBackoff)
			}
		}
		slot.inflight = nil
		close(done)
		out := s.snapshotLocked(slot, now)
		if ctx.Err() != nil {
			out.Error = "model directory request cancelled"
			out.Stale = true
		}
		s.mu.Unlock()
		return out
	}
}

// Start immediately synchronizes the pool directory and repeats every five
// minutes. It owns no persistent credentials and stops on parent cancellation.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.refreshInterval)
		defer ticker.Stop()
		s.Get(ctx, "")
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Get(ctx, "")
			}
		}
	}()
	return done
}

func (s *Service) credentials(byok string) []string {
	if byok != "" {
		return []string{byok}
	}
	s.st.Mu.RLock()
	defer s.st.Mu.RUnlock()
	now := time.Now().UnixMilli()
	keys := []string{}
	seen := map[string]bool{}
	for _, k := range s.st.Data.Keys {
		if s.st.UsableKeyLocked(k, now) && !seen[k.Key] {
			seen[k.Key] = true
			keys = append(keys, k.Key)
			if len(keys) == 3 {
				break
			}
		}
	}
	return keys
}

func (s *Service) refresh(ctx context.Context, byok string) ([]Entry, string, error) {
	keys := s.credentials(byok)
	if len(keys) == 0 {
		return nil, "", fmt.Errorf("no available key for model directory")
	}
	var lastErr error
	for _, key := range keys {
		entries, defaultModel, err := s.fetch(ctx, key)
		if err == nil {
			return entries, defaultModel, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, "", lastErr
}

func (s *Service) fetch(parent context.Context, key string) ([]Entry, string, error) {
	ctx, cancel := context.WithTimeout(parent, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(s.up.Base, "/")+"/models", nil)
	if err != nil {
		return nil, "", fmt.Errorf("invalid upstream model directory URL")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.up.HTTPClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, "", fmt.Errorf("upstream model directory request timed out or cancelled")
		}
		return nil, "", fmt.Errorf("upstream model directory request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream model directory HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("upstream model directory body read failed")
	}
	if len(raw) > maxResponseBytes {
		return nil, "", fmt.Errorf("upstream model directory exceeds size limit")
	}
	return parse(raw)
}

func validString(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsFunc(value, unicode.IsControl)
}

// parse accepts both the upstream's native {models:[id],defaultModel:id} and
// OpenAI's {data:[model]} format. Invalid entries invalidate the whole refresh;
// a successful empty list is authoritative, never merged with builtin models.
func parse(raw []byte) ([]Entry, string, error) {
	bad := func() ([]Entry, string, error) {
		return nil, "", fmt.Errorf("invalid upstream model directory response")
	}
	var envelope map[string]json.RawMessage
	if json.Unmarshal(raw, &envelope) != nil || envelope == nil {
		return bad()
	}
	if _, hasError := envelope["error"]; hasError {
		return bad()
	}
	if value, exists := envelope["success"]; exists {
		var ok bool
		if json.Unmarshal(value, &ok) != nil || !ok {
			return bad()
		}
	}
	var list []Entry
	if data, ok := envelope["data"]; ok {
		var items []map[string]json.RawMessage
		if json.Unmarshal(data, &items) != nil || items == nil {
			return bad()
		}
		for _, item := range items {
			var entry Entry
			if item == nil || json.Unmarshal(item["id"], &entry.ID) != nil || json.Unmarshal(item["object"], &entry.Object) != nil || json.Unmarshal(item["created"], &entry.Created) != nil || json.Unmarshal(item["owned_by"], &entry.OwnedBy) != nil {
				return bad()
			}
			// JSON null decodes as a zero value, so require actual scalar types.
			if string(item["created"]) == "null" || !validString(entry.ID) || entry.Object != "model" || !validString(entry.OwnedBy) || entry.Created < 0 {
				return bad()
			}
			list = append(list, entry)
		}
	} else if data, ok := envelope["models"]; ok {
		var ids []string
		if json.Unmarshal(data, &ids) != nil || ids == nil {
			return bad()
		}
		for _, id := range ids {
			if !validString(id) {
				return bad()
			}
			list = append(list, Entry{ID: id, Object: "model", OwnedBy: "meoo"})
		}
	} else {
		return bad()
	}
	out := make([]Entry, 0, len(list))
	seen := map[string]bool{}
	for _, entry := range list {
		if !seen[entry.ID] {
			seen[entry.ID] = true
			out = append(out, entry)
		}
	}
	var defaultModel string
	if value, exists := envelope["defaultModel"]; exists {
		if string(value) == "null" || json.Unmarshal(value, &defaultModel) != nil || !validString(defaultModel) {
			return bad()
		}
		if !seen[defaultModel] {
			defaultModel = ""
		}
	}
	return out, defaultModel, nil
}
