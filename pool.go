package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Picker struct {
	s      *State
	rr     int
	sticky string
}

func (p *Picker) Usable() []*Key {
	now := time.Now().UnixMilli()
	p.s.mu.RLock()
	defer p.s.mu.RUnlock()
	out := []*Key{}
	for _, k := range p.s.Keys {
		if k.Enabled && k.Key != "" && k.CooldownUntil <= now {
			out = append(out, k)
		}
	}
	return out
}

func (p *Picker) Pick() *Key {
	list := p.Usable()
	if len(list) == 0 {
		return nil
	}
	if p.s.Config.Sticky && p.sticky != "" {
		for _, k := range list {
			if k.ID == p.sticky {
				return k
			}
		}
	}
	pool := []*Key{}
	for _, k := range list {
		w := k.Weight
		if w < 1 {
			w = 1
		}
		if w > 5 {
			w = 5
		}
		for i := 0; i < w; i++ {
			pool = append(pool, k)
		}
	}
	k := pool[p.rr%len(pool)]
	p.rr++
	p.sticky = k.ID
	return k
}

func (s *State) MarkKey(k *Key, ok bool, soft bool) {
	if k == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k.Stats.Req++
	k.Stats.LastUsed = time.Now().UnixMilli()
	if ok {
		k.Stats.OK++
		k.CooldownUntil = 0
		return
	}
	k.Stats.Fail++
	cool := s.Config.HardCooldownSec
	if soft {
		cool = s.Config.CooldownSec
	}
	k.CooldownUntil = time.Now().UnixMilli() + int64(cool)*1000
}

func (s *State) Probe(k *Key) Health {
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	h := Health{CheckedAt: time.Now().UnixMilli()}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.State = "error"
		h.Error = err.Error()
	} else {
		defer resp.Body.Close()
		h.LatencyMs = time.Since(t0).Milliseconds()
		if resp.StatusCode == 200 {
			h.State = "ok"
		} else {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			h.State = "error"
			if resp.StatusCode == 401 || resp.StatusCode == 403 {
				h.State = "invalid"
			}
			h.Error = fmt.Sprintf("HTTP %d %s", resp.StatusCode, string(body))
		}
	}
	s.mu.Lock()
	k.Health = h
	if h.State == "invalid" {
		k.CooldownUntil = time.Now().UnixMilli() + int64(s.Config.HardCooldownSec)*1000
	}
	s.mu.Unlock()
	s.Touch()
	return h
}
