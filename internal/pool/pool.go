// Package pool 负责 key 池调度：权重轮询、粘性、冷却与探活。
package pool

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type Picker struct {
	St *store.Store
	Up *upstream.Client
	// RR and Sticky are protected by St.Mu, like the key/config state.
	RR             int
	Sticky         string
	stickyByCaller map[string]string
}

func cloneKey(k *model.Key) *model.Key {
	cp := *k
	if k.Account != nil {
		a := *k.Account
		cp.Account = &a
	}
	return &cp
}

func (p *Picker) usableLocked(k *model.Key, now int64) bool {
	return p.St.UsableKeyLocked(k, now)
}

// Usable returns independent snapshots: callers must not mutate shared state.
func (p *Picker) Usable() []*model.Key {
	now := time.Now().UnixMilli()
	p.St.Mu.RLock()
	defer p.St.Mu.RUnlock()
	out := []*model.Key{}
	for _, k := range p.St.Data.Keys {
		if p.usableLocked(k, now) {
			out = append(out, cloneKey(k))
		}
	}
	return out
}

func (p *Picker) Pick() *model.Key { return p.PickFor("") }

// PickFor scopes affinity to a client identity, without retaining credentials.
func (p *Picker) PickFor(callerID string) *model.Key {
	p.St.Mu.Lock()
	defer p.St.Mu.Unlock()
	now := time.Now().UnixMilli()
	list := []*model.Key{}
	total := 0
	for _, k := range p.St.Data.Keys {
		if p.usableLocked(k, now) {
			list = append(list, k)
			total += weight(k)
		}
	}
	if len(list) == 0 {
		p.Sticky = ""
		return nil
	}
	sticky := p.Sticky
	if callerID != "" {
		sticky = p.stickyByCaller[callerID]
	}
	if p.St.Data.Config.Sticky && sticky != "" {
		for _, k := range list {
			if k.ID == sticky {
				return cloneKey(k)
			}
		}
	}
	if p.RR < 0 {
		p.RR = 0
	}
	index := p.RR % total
	p.RR = (index + 1) % total
	for _, k := range list {
		index -= weight(k)
		if index < 0 {
			if callerID == "" {
				p.Sticky = k.ID
			} else if p.St.Data.Config.Sticky {
				if p.stickyByCaller == nil || len(p.stickyByCaller) >= 4096 {
					p.stickyByCaller = map[string]string{}
				}
				p.stickyByCaller[callerID] = k.ID
			}
			return cloneKey(k)
		}
	}
	return nil
}

func weight(k *model.Key) int {
	if k.Weight < 1 {
		return 1
	}
	if k.Weight > 5 {
		return 5
	}
	return k.Weight
}

// currentLocked also verifies credentials, so an in-flight result cannot update
// a deleted/replaced key. The supplied key may itself be shared (admin Probe).
func (p *Picker) currentLocked(k *model.Key) *model.Key {
	if k != nil {
		for _, current := range p.St.Data.Keys {
			if current != nil && current.ID == k.ID && current.Key == k.Key {
				return current
			}
		}
	}
	return nil
}

// RecordKey records one completed upstream operation. Cooldown is applied only
// for credential/availability failures, not caller errors or cancellations.
func (p *Picker) RecordKey(k *model.Key, ok, cooldown, soft bool, pt, ct int64) {
	p.St.Mu.Lock()
	current := p.currentLocked(k)
	if current != nil {
		current.Stats.Req++
		current.Stats.PT += pt
		current.Stats.CT += ct
		current.Stats.LastUsed = time.Now().UnixMilli()
		if ok {
			current.Stats.OK++
			// Do not clear a newer concurrent failure's cooldown on success.
		} else {
			current.Stats.Fail++
			if cooldown {
				cool := p.St.Data.Config.HardCooldownSec
				if soft {
					cool = p.St.Data.Config.CooldownSec
				}
				until := time.Now().UnixMilli() + int64(cool)*1000
				if until > current.CooldownUntil {
					current.CooldownUntil = until
				}
			}
		}
	}
	p.St.Mu.Unlock()
	if current != nil {
		p.St.Touch()
	}
}

func (p *Picker) MarkKey(k *model.Key, ok bool, soft bool) {
	p.RecordKey(k, ok, !ok, soft, 0, 0)
}

func (p *Picker) Probe(k *model.Key) model.Health { return p.ProbeContext(context.Background(), k) }

func (p *Picker) ProbeContext(parent context.Context, k *model.Key) model.Health {
	t0 := time.Now()
	h := model.Health{CheckedAt: t0.UnixMilli()}
	p.St.Mu.RLock()
	current := p.currentLocked(k)
	var snapshot *model.Key
	if current != nil {
		snapshot = cloneKey(current)
	}
	p.St.Mu.RUnlock()
	if snapshot == nil {
		h.State, h.Error = "error", "key no longer exists"
		return h
	}
	ctx, cancel := context.WithTimeout(parent, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(p.Up.Base, "/")+"/models", nil)
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+snapshot.Key)
		var resp *http.Response
		resp, err = p.Up.HTTPClient().Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
				if err == nil {
					h.State = "ok"
				}
			} else {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
				h.State = "error"
				if resp.StatusCode == 401 || resp.StatusCode == 403 {
					h.State = "invalid"
				}
				h.Error = resp.Status + " " + string(body)
			}
		}
	}
	h.LatencyMs = time.Since(t0).Milliseconds()
	if err != nil {
		h.State, h.Error = "error", err.Error()
	}
	if snapshot.Key != "" {
		h.Error = strings.ReplaceAll(h.Error, snapshot.Key, "[redacted]")
	}
	if parent.Err() != nil {
		return h
	}
	p.St.Mu.Lock()
	current = p.currentLocked(snapshot)
	if current != nil && current.Health.CheckedAt <= h.CheckedAt {
		current.Health = h
		switch h.State {
		case "ok":
			// An older probe must not clear a failure recorded while it ran.
			if current.Stats.LastUsed <= t0.UnixMilli() {
				current.CooldownUntil = 0
			}
		case "invalid":
			current.CooldownUntil = time.Now().UnixMilli() + int64(p.St.Data.Config.HardCooldownSec)*1000
		}
	}
	p.St.Mu.Unlock()
	p.St.Touch()
	return h
}
