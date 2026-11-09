// Package pool 负责 key 池调度：权重轮询、粘性、冷却与探活。
package pool

import (
	"context"
	"io"
	"net/http"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type Picker struct {
	St     *store.Store
	Up     *upstream.Client
	RR     int
	Sticky string
}

func (p *Picker) Usable() []*model.Key {
	now := time.Now().UnixMilli()
	p.St.Mu.RLock()
	defer p.St.Mu.RUnlock()
	out := []*model.Key{}
	for _, k := range p.St.Data.Keys {
		if k.Enabled && k.Key != "" && k.CooldownUntil <= now {
			out = append(out, k)
		}
	}
	return out
}

func (p *Picker) Pick() *model.Key {
	list := p.Usable()
	if len(list) == 0 {
		return nil
	}
	if p.St.Data.Config.Sticky && p.Sticky != "" {
		for _, k := range list {
			if k.ID == p.Sticky {
				return k
			}
		}
	}
	expanded := []*model.Key{}
	for _, k := range list {
		w := k.Weight
		if w < 1 {
			w = 1
		}
		if w > 5 {
			w = 5
		}
		for i := 0; i < w; i++ {
			expanded = append(expanded, k)
		}
	}
	k := expanded[p.RR%len(expanded)]
	p.RR++
	p.Sticky = k.ID
	return k
}

func (p *Picker) MarkKey(k *model.Key, ok bool, soft bool) {
	if k == nil {
		return
	}
	p.St.Mu.Lock()
	defer p.St.Mu.Unlock()
	k.Stats.Req++
	k.Stats.LastUsed = time.Now().UnixMilli()
	if ok {
		k.Stats.OK++
		k.CooldownUntil = 0
		return
	}
	k.Stats.Fail++
	cool := p.St.Data.Config.HardCooldownSec
	if soft {
		cool = p.St.Data.Config.CooldownSec
	}
	k.CooldownUntil = time.Now().UnixMilli() + int64(cool)*1000
}

func (p *Picker) Probe(k *model.Key) model.Health {
	t0 := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.Up.Base+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+k.Key)
	h := model.Health{CheckedAt: time.Now().UnixMilli()}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.State, h.Error = "error", err.Error()
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
			h.Error = resp.Status + " " + string(body)
		}
	}
	p.St.Mu.Lock()
	k.Health = h
	if h.State == "invalid" {
		k.CooldownUntil = time.Now().UnixMilli() + int64(p.St.Data.Config.HardCooldownSec)*1000
	}
	p.St.Mu.Unlock()
	p.St.Touch()
	return h
}
