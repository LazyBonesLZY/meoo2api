package main

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	tokens   = map[string]int64{}
	tokensMu sync.Mutex
)

func issueToken() string {
	t := randID("", 24)
	tokensMu.Lock()
	tokens[t] = time.Now().Add(12 * time.Hour).UnixMilli()
	tokensMu.Unlock()
	return t
}

func adminOK(r *http.Request) bool {
	t := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	t = strings.TrimPrefix(t, "admin:")
	tokensMu.Lock()
	exp, ok := tokens[t]
	tokensMu.Unlock()
	if !ok {
		return false
	}
	if exp < time.Now().UnixMilli() {
		tokensMu.Lock()
		delete(tokens, t)
		tokensMu.Unlock()
		return false
	}
	return true
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

func (s *State) Alerts() []map[string]string {
	out := []map[string]string{}
	now := time.Now().UnixMilli()
	bad, cooling, usable := 0, 0, 0
	for _, k := range s.Keys {
		if k.Health.State == "invalid" {
			bad++
		}
		if k.CooldownUntil > now {
			cooling++
		}
		if k.Enabled && k.CooldownUntil <= now && k.Key != "" {
			usable++
		}
	}
	if bad > 0 {
		out = append(out, map[string]string{"level": "bad", "text": strconv.Itoa(bad) + " 个 key 鉴权失败"})
	}
	if cooling > 0 {
		out = append(out, map[string]string{"level": "warn", "text": strconv.Itoa(cooling) + " 个 key 处于冷却中"})
	}
	if usable == 0 && len(s.Keys) > 0 {
		out = append(out, map[string]string{"level": "bad", "text": "没有可用 key，请求会失败"})
	}
	if len(s.Keys) == 0 {
		out = append(out, map[string]string{"level": "warn", "text": "key 池为空，只能 BYOK"})
	}
	return out
}

func (s *State) AdminRoute(w http.ResponseWriter, r *http.Request, path string, p *Picker) bool {
	if !strings.HasPrefix(path, "/admin/api/") {
		return false
	}
	if path == "/admin/api/login" && r.Method == http.MethodPost {
		var in struct{ Username, Password string }
		_ = readJSON(r, &in)
		if in.Username == "" {
			in.Username = "admin"
		}
		if in.Username == s.Config.AdminUser && shaHex(in.Password) == s.Config.AdminPass {
			writeJSON(w, 200, map[string]any{"ok": true, "token": issueToken()})
		} else {
			writeJSON(w, 401, map[string]any{"ok": false, "error": "用户名或密码错误"})
		}
		return true
	}
	if !adminOK(r) {
		writeJSON(w, 401, map[string]any{"error": "unauthorized"})
		return true
	}
	reveal := r.URL.Query().Get("reveal") == "1"
	now := time.Now().UnixMilli()

	switch {
	case path == "/admin/api/state" && r.Method == http.MethodGet:
		s.mu.RLock()
		keys := []map[string]any{}
		for _, k := range s.Keys {
			kv := Mask(k.Key)
			if reveal {
				kv = k.Key
			}
			keys = append(keys, map[string]any{
				"id": k.ID, "label": k.Label, "key": kv, "enabled": k.Enabled, "weight": k.Weight,
				"note": k.Note, "createdAt": k.CreatedAt, "health": k.Health, "stats": k.Stats,
				"cooldownUntil": k.CooldownUntil,
				"cooling":       max64(0, (k.CooldownUntil-now)/1000),
			})
		}
		cks := []map[string]any{}
		for _, c := range s.ClientKeys {
			kv := c.Key
			if !reveal && len(kv) > 10 {
				kv = kv[:6] + "…" + kv[len(kv)-4:]
			}
			cks = append(cks, map[string]any{
				"id": c.ID, "label": c.Label, "key": kv, "enabled": c.Enabled,
				"createdAt": c.CreatedAt, "stats": c.Stats,
			})
		}
		cis := []map[string]any{}
		for _, c := range s.Checkins {
			ck := c.Cookie
			if !reveal && len(ck) > 12 {
				ck = ck[:8] + "…(" + strconv.Itoa(len(ck)) + ")"
			}
			cis = append(cis, map[string]any{
				"id": c.ID, "label": c.Label, "cookie": ck, "enabled": c.Enabled,
				"createdAt": c.CreatedAt, "lastGrantDate": c.LastGrantDate,
				"totalGranted": c.TotalGranted, "grantCount": c.GrantCount, "last": c.Last,
			})
		}
		cfg := map[string]any{
			"timeoutMs": s.Config.TimeoutMs, "defaultMaxTokens": s.Config.DefaultMaxTokens,
			"cooldownSec": s.Config.CooldownSec, "hardCooldownSec": s.Config.HardCooldownSec,
			"sticky": s.Config.Sticky, "maxLogs": s.Config.MaxLogs, "maxUsageHours": s.Config.MaxUsageHours,
			"aliases": s.Config.Aliases, "hiddenModels": s.Config.HiddenModels,
			"adminUser": s.Config.AdminUser, "autoCheckin": s.Config.AutoCheckin,
			"checkinIntervalH": s.Config.CheckinIntervalH,
		}
		s.mu.RUnlock()
		writeJSON(w, 200, map[string]any{
			"config": cfg, "keys": keys, "clientKeys": cks, "checkins": cis,
			"models": NativeModels, "alerts": s.Alerts(), "uptime": time.Since(startedAt).Seconds(),
		})
		return true

	case path == "/admin/api/keys" && r.Method == http.MethodPost:
		var in struct {
			Label  string `json:"label"`
			Key    string `json:"key"`
			Weight int    `json:"weight"`
		}
		_ = readJSON(r, &in)
		if !IsProjectAK(strings.TrimSpace(in.Key)) {
			writeJSON(w, 400, map[string]any{"error": "项目服务 AK 格式不对（meoo_project_…）"})
			return true
		}
		if in.Label == "" {
			in.Label = "key"
		}
		k := NewKey(in.Label, strings.TrimSpace(in.Key), in.Weight)
		s.mu.Lock()
		s.Keys = append(s.Keys, k)
		s.mu.Unlock()
		s.Touch()
		_ = s.Save()
		writeJSON(w, 200, map[string]any{"id": k.ID, "label": k.Label, "key": Mask(k.Key)})
		return true

	case path == "/admin/api/probe-all" && r.Method == http.MethodPost:
		s.mu.RLock()
		list := append([]*Key(nil), s.Keys...)
		s.mu.RUnlock()
		results := []map[string]any{}
		for _, k := range list {
			h := s.Probe(k)
			results = append(results, map[string]any{"id": k.ID, "label": k.Label, "state": h.State, "latencyMs": h.LatencyMs, "error": h.Error})
		}
		_ = s.Save()
		writeJSON(w, 200, map[string]any{"results": results})
		return true

	case strings.HasPrefix(path, "/admin/api/keys/"):
		rest := strings.TrimPrefix(path, "/admin/api/keys/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = "/" + parts[1]
		}
		s.mu.Lock()
		var k *Key
		for _, x := range s.Keys {
			if x.ID == id {
				k = x
			}
		}
		s.mu.Unlock()
		if k == nil {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return true
		}
		switch {
		case sub == "/probe" && r.Method == http.MethodPost:
			h := s.Probe(k)
			writeJSON(w, 200, h)
		case sub == "/reveal":
			writeJSON(w, 200, map[string]any{"key": k.Key})
		case r.Method == http.MethodPatch:
			var in map[string]any
			_ = readJSON(r, &in)
			s.mu.Lock()
			if v, ok := in["label"].(string); ok {
				k.Label = v
			}
			if v, ok := in["note"].(string); ok {
				k.Note = v
			}
			if v, ok := in["weight"].(float64); ok && v >= 1 {
				k.Weight = int(v)
			}
			if v, ok := in["enabled"].(bool); ok {
				k.Enabled = v
			}
			if v, ok := in["key"].(string); ok && IsProjectAK(v) {
				k.Key = v
			}
			if v, ok := in["resetCooldown"].(bool); ok && v {
				k.CooldownUntil = 0
			}
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		case r.Method == http.MethodDelete:
			s.mu.Lock()
			out := s.Keys[:0]
			for _, x := range s.Keys {
				if x.ID != id {
					out = append(out, x)
				}
			}
			s.Keys = out
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		default:
			writeJSON(w, 404, map[string]any{"error": "unknown"})
		}
		return true

	case path == "/admin/api/client-keys" && r.Method == http.MethodPost:
		var in struct{ Label, Key string }
		_ = readJSON(r, &in)
		if in.Label == "" {
			in.Label = "默认"
		}
		c := NewClientKey(in.Label, in.Key)
		s.mu.Lock()
		s.ClientKeys = append(s.ClientKeys, c)
		s.mu.Unlock()
		s.Touch()
		_ = s.Save()
		writeJSON(w, 200, map[string]any{"id": c.ID, "label": c.Label, "key": c.Key})
		return true

	case strings.HasPrefix(path, "/admin/api/client-keys/"):
		rest := strings.TrimPrefix(path, "/admin/api/client-keys/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = "/" + parts[1]
		}
		s.mu.Lock()
		var c *ClientKey
		for _, x := range s.ClientKeys {
			if x.ID == id {
				c = x
			}
		}
		s.mu.Unlock()
		if c == nil {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return true
		}
		if sub == "/reveal" {
			writeJSON(w, 200, map[string]any{"key": c.Key})
			return true
		}
		switch r.Method {
		case http.MethodPatch:
			var in map[string]any
			_ = readJSON(r, &in)
			s.mu.Lock()
			if v, ok := in["label"].(string); ok {
				c.Label = v
			}
			if v, ok := in["enabled"].(bool); ok {
				c.Enabled = v
			}
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		case http.MethodDelete:
			s.mu.Lock()
			out := s.ClientKeys[:0]
			for _, x := range s.ClientKeys {
				if x.ID != id {
					out = append(out, x)
				}
			}
			s.ClientKeys = out
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		}
		return true

	case path == "/admin/api/checkins":
		if r.Method == http.MethodGet {
			s.mu.RLock()
			list := []map[string]any{}
			for _, c := range s.Checkins {
				ck := c.Cookie
				if len(ck) > 12 {
					ck = ck[:8] + "…(" + strconv.Itoa(len(ck)) + ")"
				}
				list = append(list, map[string]any{"id": c.ID, "label": c.Label, "cookie": ck, "enabled": c.Enabled, "last": c.Last})
			}
			s.mu.RUnlock()
			writeJSON(w, 200, map[string]any{"checkins": list})
			return true
		}
		var in struct{ Label, Cookie string }
		_ = readJSON(r, &in)
		if strings.TrimSpace(in.Cookie) == "" {
			writeJSON(w, 400, map[string]any{"error": "缺少 Cookie"})
			return true
		}
		if in.Label == "" {
			in.Label = "我的账号"
		}
		c := &Checkin{ID: randID("ci_", 6), Label: in.Label, Cookie: strings.TrimSpace(in.Cookie), Enabled: true, CreatedAt: time.Now().UnixMilli()}
		s.mu.Lock()
		s.Checkins = append(s.Checkins, c)
		s.mu.Unlock()
		s.Touch()
		_ = s.Save()
		writeJSON(w, 200, map[string]any{"id": c.ID, "label": c.Label})
		return true

	case strings.HasPrefix(path, "/admin/api/checkins/"):
		rest := strings.TrimPrefix(path, "/admin/api/checkins/")
		parts := strings.SplitN(rest, "/", 2)
		id := parts[0]
		sub := ""
		if len(parts) > 1 {
			sub = "/" + parts[1]
		}
		s.mu.Lock()
		var c *Checkin
		for _, x := range s.Checkins {
			if x.ID == id {
				c = x
			}
		}
		s.mu.Unlock()
		if c == nil {
			writeJSON(w, 404, map[string]any{"error": "not found"})
			return true
		}
		switch {
		case sub == "/run" && r.Method == http.MethodPost:
			writeJSON(w, 200, RunCheckin(s, c))
		case sub == "/reveal":
			writeJSON(w, 200, map[string]any{"cookie": c.Cookie})
		case r.Method == http.MethodPatch:
			var in map[string]any
			_ = readJSON(r, &in)
			s.mu.Lock()
			if v, ok := in["label"].(string); ok {
				c.Label = v
			}
			if v, ok := in["enabled"].(bool); ok {
				c.Enabled = v
			}
			if v, ok := in["cookie"].(string); ok && v != "" {
				c.Cookie = v
			}
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		case r.Method == http.MethodDelete:
			s.mu.Lock()
			out := s.Checkins[:0]
			for _, x := range s.Checkins {
				if x.ID != id {
					out = append(out, x)
				}
			}
			s.Checkins = out
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
		}
		return true

	case path == "/admin/api/checkins-run-all" && r.Method == http.MethodPost:
		writeJSON(w, 200, map[string]any{"results": RunAllCheckins(s, true)})
		return true

	case path == "/admin/api/aliases":
		if r.Method == http.MethodPost {
			var in struct{ Name, Target string }
			_ = readJSON(r, &in)
			if in.Name == "" || in.Target == "" {
				writeJSON(w, 400, map[string]any{"error": "缺 name 或 target"})
				return true
			}
			s.mu.Lock()
			s.Config.Aliases[strings.ToLower(in.Name)] = in.Target
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
			return true
		}
		if r.Method == http.MethodDelete {
			name := strings.ToLower(r.URL.Query().Get("name"))
			s.mu.Lock()
			delete(s.Config.Aliases, name)
			s.mu.Unlock()
			s.Touch()
			_ = s.Save()
			writeJSON(w, 200, map[string]any{"ok": true})
			return true
		}

	case path == "/admin/api/config" && r.Method == http.MethodPatch:
		var in map[string]any
		_ = readJSON(r, &in)
		s.mu.Lock()
		num := func(k string, dst *int) {
			if v, ok := in[k].(float64); ok && v > 0 {
				*dst = int(v)
			}
		}
		num("timeoutMs", &s.Config.TimeoutMs)
		num("defaultMaxTokens", &s.Config.DefaultMaxTokens)
		num("cooldownSec", &s.Config.CooldownSec)
		num("hardCooldownSec", &s.Config.HardCooldownSec)
		num("maxLogs", &s.Config.MaxLogs)
		num("maxUsageHours", &s.Config.MaxUsageHours)
		num("checkinIntervalH", &s.Config.CheckinIntervalH)
		if v, ok := in["sticky"].(bool); ok {
			s.Config.Sticky = v
		}
		if v, ok := in["autoCheckin"].(bool); ok {
			s.Config.AutoCheckin = v
		}
		if v, ok := in["hiddenModels"].([]any); ok {
			list := []string{}
			for _, x := range v {
				if sv, ok := x.(string); ok {
					list = append(list, sv)
				}
			}
			s.Config.HiddenModels = list
		}
		if v, ok := in["adminPassword"].(string); ok && v != "" {
			s.Config.AdminPass = shaHex(v)
			tokensMu.Lock()
			tokens = map[string]int64{}
			tokensMu.Unlock()
		}
		s.mu.Unlock()
		s.Touch()
		_ = s.Save()
		writeJSON(w, 200, map[string]any{"ok": true})
		return true

	case path == "/admin/api/logs" && r.Method == http.MethodGet:
		limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
		if limit > 500 {
			limit = 500
		}
		status := r.URL.Query().Get("status")
		model := r.URL.Query().Get("model")
		q := r.URL.Query().Get("q")
		s.mu.RLock()
		rows := []*LogRow{}
		for _, row := range s.Logs {
			if status == "ok" && row.Status >= 400 {
				continue
			}
			if status == "fail" && row.Status < 400 {
				continue
			}
			if model != "" && row.Model != model {
				continue
			}
			if q != "" && !strings.Contains(row.Rid, q) && !strings.Contains(row.KeyLabel, q) {
				continue
			}
			rows = append(rows, row)
			if len(rows) >= limit {
				break
			}
		}
		total := len(s.Logs)
		s.mu.RUnlock()
		writeJSON(w, 200, map[string]any{"total": total, "rows": rows})
		return true

	case path == "/admin/api/usage" && r.Method == http.MethodGet:
		hours := parseIntDefault(r.URL.Query().Get("hours"), 24)
		s.mu.RLock()
		keys := []string{}
		for k := range s.Usage {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > hours {
			keys = keys[len(keys)-hours:]
		}
		out := []map[string]any{}
		for _, k := range keys {
			b := s.Usage[k]
			out = append(out, map[string]any{
				"hour": k, "req": b.Req, "ok": b.OK, "fail": b.Fail, "pt": b.PT, "ct": b.CT,
				"p50": Percentile(b.Lat, 50), "p95": Percentile(b.Lat, 95), "models": b.Models,
			})
		}
		s.mu.RUnlock()
		writeJSON(w, 200, map[string]any{"hours": out})
		return true

	case path == "/admin/api/export" && r.Method == http.MethodGet:
		s.mu.RLock()
		cfg := map[string]any{
			"timeoutMs": s.Config.TimeoutMs, "defaultMaxTokens": s.Config.DefaultMaxTokens,
			"cooldownSec": s.Config.CooldownSec, "hardCooldownSec": s.Config.HardCooldownSec,
			"sticky": s.Config.Sticky, "maxLogs": s.Config.MaxLogs, "maxUsageHours": s.Config.MaxUsageHours,
			"aliases": s.Config.Aliases, "hiddenModels": s.Config.HiddenModels,
			"adminUser": s.Config.AdminUser, "autoCheckin": s.Config.AutoCheckin,
			"checkinIntervalH": s.Config.CheckinIntervalH,
		}
		cis := []map[string]any{}
		for _, c := range s.Checkins {
			cis = append(cis, map[string]any{
				"id": c.ID, "label": c.Label, "cookie": "", "enabled": c.Enabled,
				"createdAt": c.CreatedAt, "lastGrantDate": c.LastGrantDate,
				"totalGranted": c.TotalGranted, "grantCount": c.GrantCount,
			})
		}
		payload := map[string]any{
			"version": s.Version, "config": cfg, "keys": s.Keys, "clientKeys": s.ClientKeys,
			"checkins": cis, "logs": s.Logs, "usage": s.Usage,
		}
		s.mu.RUnlock()
		writeJSON(w, 200, payload)
		return true
	}

	writeJSON(w, 404, map[string]any{"error": "unknown admin path"})
	return true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var startedAt = time.Now()

func init() {
	_ = os.Getenv("MEOO_AI_BASE")
}
