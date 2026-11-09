// Package admin 提供控制台 API：鉴权、key 池、签到、日志与用量。
package admin

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/checkin"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
)

type Tokens struct {
	mu   sync.Mutex
	data map[string]int64
}

func NewTokens() *Tokens { return &Tokens{data: map[string]int64{}} }

func (t *Tokens) issue() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	t.data[tok] = time.Now().Add(12 * time.Hour).UnixMilli()
	t.mu.Unlock()
	return tok
}

func (t *Tokens) valid(r *http.Request) bool {
	v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	v = strings.TrimPrefix(v, "admin:")
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.data[v]
	if !ok {
		return false
	}
	if exp < time.Now().UnixMilli() {
		delete(t.data, v)
		return false
	}
	return true
}

func (t *Tokens) Clear() {
	t.mu.Lock()
	t.data = map[string]int64{}
	t.mu.Unlock()
}

type Handler struct {
	St     *store.Store
	Picker *pool.Picker
	Acc    *account.Service
	Tokens *Tokens
	Start  time.Time
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readBody(r *http.Request, v any) {
	defer r.Body.Close()
	_ = json.NewDecoder(r.Body).Decode(v)
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimRight(r.URL.Path, "/")
	if !strings.HasPrefix(path, "/admin/api/") {
		return false
	}
	if path == "/admin/api/login" && r.Method == http.MethodPost {
		var in struct{ Username, Password string }
		readBody(r, &in)
		if in.Username == "" {
			in.Username = "admin"
		}
		h.St.Mu.RLock()
		ok := in.Username == h.St.Data.Config.AdminUser && model.ShaHex(in.Password) == h.St.Data.Config.AdminPass
		h.St.Mu.RUnlock()
		if ok {
			write(w, 200, map[string]any{"ok": true, "token": h.Tokens.issue()})
		} else {
			write(w, 401, map[string]any{"ok": false, "error": "用户名或密码错误"})
		}
		return true
	}
	if !h.Tokens.valid(r) {
		write(w, 401, map[string]any{"error": "unauthorized"})
		return true
	}
	h.route(w, r, path, r.URL.Query().Get("reveal") == "1")
	return true
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, path string, reveal bool) {
	now := time.Now().UnixMilli()
	switch {
	case path == "/admin/api/state" && r.Method == http.MethodGet:
		h.state(w, reveal, now)
	case path == "/admin/api/keys" && r.Method == http.MethodPost:
		var in struct {
			Label  string `json:"label"`
			Key    string `json:"key"`
			Weight int    `json:"weight"`
		}
		readBody(r, &in)
		if !strings.HasPrefix(strings.TrimSpace(in.Key), "meoo_project_") {
			write(w, 400, map[string]any{"error": "项目服务 AK 格式不对（meoo_project_…）"})
			return
		}
		if in.Label == "" {
			in.Label = "key"
		}
		k := store.NewKey(in.Label, strings.TrimSpace(in.Key), in.Weight)
		h.St.Mu.Lock()
		h.St.Data.Keys = append(h.St.Data.Keys, k)
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"id": k.ID, "label": k.Label, "key": model.Mask(k.Key)})
	case path == "/admin/api/probe-all" && r.Method == http.MethodPost:
		h.St.Mu.RLock()
		list := append([]*model.Key(nil), h.St.Data.Keys...)
		h.St.Mu.RUnlock()
		out := []map[string]any{}
		for _, k := range list {
			hh := h.Picker.Probe(k)
			out = append(out, map[string]any{"id": k.ID, "label": k.Label, "state": hh.State, "latencyMs": hh.LatencyMs, "error": hh.Error})
		}
		_ = h.St.Save()
		write(w, 200, map[string]any{"results": out})
	case strings.HasPrefix(path, "/admin/api/keys/"):
		h.keyRoute(w, r, strings.TrimPrefix(path, "/admin/api/keys/"))
	case path == "/admin/api/client-keys" && r.Method == http.MethodPost:
		var in struct{ Label, Key string }
		readBody(r, &in)
		if in.Label == "" {
			in.Label = "默认"
		}
		c := store.NewClientKey(in.Label, in.Key)
		h.St.Mu.Lock()
		h.St.Data.ClientKeys = append(h.St.Data.ClientKeys, c)
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"id": c.ID, "label": c.Label, "key": c.Key})
	case strings.HasPrefix(path, "/admin/api/client-keys/"):
		h.clientKeyRoute(w, r, strings.TrimPrefix(path, "/admin/api/client-keys/"))
	case path == "/admin/api/accounts":
		h.accountRoot(w, r)
	case strings.HasPrefix(path, "/admin/api/accounts/"):
		h.accountRoute(w, r, strings.TrimPrefix(path, "/admin/api/accounts/"))
	case path == "/admin/api/accounts-checkin-all" && r.Method == http.MethodPost:
		write(w, 200, map[string]any{"results": checkin.RunAll(h.St, true)})
	case path == "/admin/api/aliases":
		h.aliasRoute(w, r)
	case path == "/admin/api/config" && r.Method == http.MethodPatch:
		h.configRoute(w, r)
	case path == "/admin/api/logs" && r.Method == http.MethodGet:
		h.logsRoute(w, r)
	case path == "/admin/api/usage" && r.Method == http.MethodGet:
		h.usageRoute(w, r)
	case path == "/admin/api/export" && r.Method == http.MethodGet:
		h.exportRoute(w)
	default:
		write(w, 404, map[string]any{"error": "unknown admin path"})
	}
}

func (h *Handler) state(w http.ResponseWriter, reveal bool, now int64) {
	h.St.Mu.RLock()
	keys := []map[string]any{}
	for _, k := range h.St.Data.Keys {
		kv := model.Mask(k.Key)
		if reveal {
			kv = k.Key
		}
		keys = append(keys, map[string]any{
			"id": k.ID, "label": k.Label, "key": kv, "enabled": k.Enabled, "weight": k.Weight,
			"note": k.Note, "createdAt": k.CreatedAt, "health": k.Health, "stats": k.Stats,
			"cooldownUntil": k.CooldownUntil, "account": k.Account, "accountId": k.AccountID,
			"cooling": max64(0, (k.CooldownUntil-now)/1000),
		})
	}
	cks := []map[string]any{}
	for _, c := range h.St.Data.ClientKeys {
		kv := c.Key
		if !reveal && len(kv) > 10 {
			kv = kv[:6] + "…" + kv[len(kv)-4:]
		}
		cks = append(cks, map[string]any{"id": c.ID, "label": c.Label, "key": kv,
			"enabled": c.Enabled, "createdAt": c.CreatedAt, "stats": c.Stats})
	}
	acs := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		ck := a.Cookie
		if !reveal && len(ck) > 12 {
			ck = ck[:8] + "…(" + strconv.Itoa(len(ck)) + ")"
		}
		pks := []map[string]any{}
		for _, id := range a.PersonalKeyIDs {
			for _, pk := range h.St.Data.PersonalKeys {
				if pk.ID == id {
					pks = append(pks, map[string]any{"id": pk.ID, "key": model.Mask(pk.Key), "scopes": len(pk.Scopes)})
				}
			}
		}
		projs := []map[string]any{}
		for _, id := range a.ProjectKeyIDs {
			for _, k := range h.St.Data.Keys {
				if k.ID == id {
					projs = append(projs, map[string]any{"id": k.ID, "label": k.Label, "key": model.Mask(k.Key), "health": k.Health.State})
				}
			}
		}
		acs = append(acs, map[string]any{
			"id": a.ID, "label": a.Label, "userId": a.UserID, "name": a.Name, "cookie": ck,
			"enabled": a.Enabled, "createdAt": a.CreatedAt, "lastPing": a.LastPing,
			"pingOk": a.PingOK, "expired": a.Expired, "lastGrantDate": a.LastGrantDate,
			"totalGranted": a.TotalGranted, "grantCount": a.GrantCount, "last": a.Last,
			"personalKeys": pks, "projectKeys": projs,
		})
	}
	pks := []map[string]any{}
	for _, p := range h.St.Data.PersonalKeys {
		kv := model.Mask(p.Key)
		if reveal {
			kv = p.Key
		}
		pks = append(pks, map[string]any{"id": p.ID, "accountId": p.AccountID, "name": p.Name,
			"key": kv, "scopes": p.Scopes, "createdAt": p.CreatedAt})
	}
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	out := map[string]any{
		"config":       cfg,
		"keys":         keys,
		"clientKeys":   cks,
		"accounts":     acs,
		"personalKeys": pks,
		"lastAutoRun":  h.St.Data.LastAutoRun,
		"models":       model.NativeModels,
		"alerts":       h.St.AlertsLocked(),
		"uptime":       time.Since(h.Start).Seconds(),
	}
	h.St.Mu.RUnlock()
	write(w, 200, out)
}

func (h *Handler) keyRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = "/" + parts[1]
	}
	var k *model.Key
	h.St.Mu.RLock()
	for _, x := range h.St.Data.Keys {
		if x.ID == id {
			k = x
		}
	}
	h.St.Mu.RUnlock()
	if k == nil {
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	switch {
	case sub == "/probe" && r.Method == http.MethodPost:
		write(w, 200, h.Picker.Probe(k))
	case sub == "/reveal":
		write(w, 200, map[string]any{"key": k.Key})
	case sub == "/identify" && r.Method == http.MethodPost:
		acc, err := h.Acc.IdentifyKey(k)
		if err != nil {
			write(w, 400, map[string]any{"error": err.Error()})
		} else {
			_ = h.St.Save()
			write(w, 200, map[string]any{"account": acc})
		}
	case r.Method == http.MethodPatch:
		var in map[string]any
		readBody(r, &in)
		h.St.Mu.Lock()
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
		if v, ok := in["key"].(string); ok && strings.HasPrefix(v, "meoo_project_") {
			k.Key = v
		}
		if v, ok := in["resetCooldown"].(bool); ok && v {
			k.CooldownUntil = 0
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodDelete:
		h.St.Mu.Lock()
		out := h.St.Data.Keys[:0]
		for _, x := range h.St.Data.Keys {
			if x.ID != id {
				out = append(out, x)
			}
		}
		h.St.Data.Keys = out
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	default:
		write(w, 404, map[string]any{"error": "unknown"})
	}
}

func (h *Handler) clientKeyRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = "/" + parts[1]
	}
	var c *model.ClientKey
	h.St.Mu.RLock()
	for _, x := range h.St.Data.ClientKeys {
		if x.ID == id {
			c = x
		}
	}
	h.St.Mu.RUnlock()
	if c == nil {
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	if sub == "/reveal" {
		write(w, 200, map[string]any{"key": c.Key})
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var in map[string]any
		readBody(r, &in)
		h.St.Mu.Lock()
		if v, ok := in["label"].(string); ok {
			c.Label = v
		}
		if v, ok := in["enabled"].(bool); ok {
			c.Enabled = v
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	case http.MethodDelete:
		h.St.Mu.Lock()
		out := h.St.Data.ClientKeys[:0]
		for _, x := range h.St.Data.ClientKeys {
			if x.ID != id {
				out = append(out, x)
			}
		}
		h.St.Data.ClientKeys = out
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	}
}

func (h *Handler) accountRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.St.Mu.RLock()
		list := []map[string]any{}
		for _, a := range h.St.Data.Accounts {
			list = append(list, map[string]any{"id": a.ID, "label": a.Label, "userId": a.UserID,
				"name": a.Name, "enabled": a.Enabled, "expired": a.Expired, "pingOk": a.PingOK})
		}
		h.St.Mu.RUnlock()
		write(w, 200, map[string]any{"accounts": list})
		return
	}
	var in struct{ Label, Cookie string }
	readBody(r, &in)
	if strings.TrimSpace(in.Cookie) == "" {
		write(w, 400, map[string]any{"error": "缺少 Cookie"})
		return
	}
	if in.Label == "" {
		in.Label = "未命名"
	}
	a := &model.Account{ID: model.RandID("a_", 6), Label: in.Label, Cookie: strings.TrimSpace(in.Cookie),
		Enabled: true, CreatedAt: time.Now().UnixMilli(), PersonalKeyIDs: []string{}, ProjectKeyIDs: []string{}}
	h.St.Mu.Lock()
	h.St.Data.Accounts = append(h.St.Data.Accounts, a)
	h.St.Mu.Unlock()
	_ = h.Acc.Identify(a)
	_, _ = h.Acc.Ping(a)
	_ = h.St.Save()
	if h.St.Data.Config.AutoOnboard {
		go func() { _, _ = h.Acc.Onboard(a) }()
	}
	write(w, 200, map[string]any{"id": a.ID, "label": a.Label, "userId": a.UserID, "name": a.Name})
}

func (h *Handler) accountRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) > 1 {
		sub = "/" + parts[1]
	}
	a := h.Acc.Find(id)
	if a == nil {
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	switch {
	case sub == "/identify" && r.Method == http.MethodPost:
		if err := h.Acc.Identify(a); err != nil {
			write(w, 400, map[string]any{"error": err.Error()})
			return
		}
		_ = h.St.Save()
		write(w, 200, map[string]any{"userId": a.UserID, "name": a.Name})
	case sub == "/ping" && r.Method == http.MethodPost:
		ok, err := h.Acc.Ping(a)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": ok, "error": msg, "lastPing": a.LastPing})
	case sub == "/checkin" && r.Method == http.MethodPost:
		write(w, 200, checkin.Run(h.St, a))
	case sub == "/personal-key" && r.Method == http.MethodPost:
		pk, err := h.Acc.PersonalKey(a)
		if err != nil {
			write(w, 400, map[string]any{"error": err.Error()})
			return
		}
		write(w, 200, map[string]any{"id": pk.ID, "key": pk.Key, "scopes": len(pk.Scopes),
			"note": "个人 AK：可调 CLI / Open API，不能直接进模型池"})
	case sub == "/onboard" && r.Method == http.MethodPost:
		out, err := h.Acc.Onboard(a)
		if err != nil {
			code := 400
			if out != nil {
				code = 200
			}
			write(w, code, map[string]any{"error": err.Error(), "partial": out})
			return
		}
		write(w, 200, out)
	case sub == "/cookie":
		write(w, 200, map[string]any{"cookie": a.Cookie})
	case r.Method == http.MethodPatch:
		var in map[string]any
		readBody(r, &in)
		h.St.Mu.Lock()
		if v, ok := in["label"].(string); ok {
			a.Label = v
		}
		if v, ok := in["enabled"].(bool); ok {
			a.Enabled = v
		}
		if v, ok := in["cookie"].(string); ok && v != "" {
			a.Cookie = v
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodDelete:
		h.St.Mu.Lock()
		out := h.St.Data.Accounts[:0]
		for _, x := range h.St.Data.Accounts {
			if x.ID != id {
				out = append(out, x)
			}
		}
		h.St.Data.Accounts = out
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	default:
		write(w, 404, map[string]any{"error": "unknown"})
	}
}

func (h *Handler) aliasRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var in struct{ Name, Target string }
		readBody(r, &in)
		if in.Name == "" || in.Target == "" {
			write(w, 400, map[string]any{"error": "缺 name 或 target"})
			return
		}
		h.St.Mu.Lock()
		h.St.Data.Config.Aliases[strings.ToLower(in.Name)] = in.Target
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
		return
	}
	if r.Method == http.MethodDelete {
		name := strings.ToLower(r.URL.Query().Get("name"))
		h.St.Mu.Lock()
		delete(h.St.Data.Config.Aliases, name)
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
		return
	}
	write(w, 405, map[string]any{"error": "method"})
}

func (h *Handler) configRoute(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	readBody(r, &in)
	h.St.Mu.Lock()
	num := func(k string, dst *int) {
		if v, ok := in[k].(float64); ok && v > 0 {
			*dst = int(v)
		}
	}
	num("timeoutMs", &h.St.Data.Config.TimeoutMs)
	num("defaultMaxTokens", &h.St.Data.Config.DefaultMaxTokens)
	num("cooldownSec", &h.St.Data.Config.CooldownSec)
	num("hardCooldownSec", &h.St.Data.Config.HardCooldownSec)
	num("maxLogs", &h.St.Data.Config.MaxLogs)
	num("maxUsageHours", &h.St.Data.Config.MaxUsageHours)
	num("checkinHour", &h.St.Data.Config.CheckinHour)
	num("keepAliveHours", &h.St.Data.Config.KeepAliveHours)
	if v, ok := in["sticky"].(bool); ok {
		h.St.Data.Config.Sticky = v
	}
	if v, ok := in["autoCheckin"].(bool); ok {
		h.St.Data.Config.AutoCheckin = v
	}
	if v, ok := in["autoOnboard"].(bool); ok {
		h.St.Data.Config.AutoOnboard = v
	}
	if v, ok := in["hiddenModels"].([]any); ok {
		list := []string{}
		for _, x := range v {
			if sv, ok := x.(string); ok {
				list = append(list, sv)
			}
		}
		h.St.Data.Config.HiddenModels = list
	}
	if v, ok := in["adminPassword"].(string); ok && v != "" {
		h.St.Data.Config.AdminPass = model.ShaHex(v)
		h.Tokens.Clear()
	}
	h.St.Mu.Unlock()
	_ = h.St.Save()
	write(w, 200, map[string]any{"ok": true})
}

func (h *Handler) logsRoute(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 500 {
		limit = 500
	}
	status, mdl, q := r.URL.Query().Get("status"), r.URL.Query().Get("model"), r.URL.Query().Get("q")
	h.St.Mu.RLock()
	rows := []*model.LogRow{}
	for _, row := range h.St.Data.Logs {
		if status == "ok" && row.Status >= 400 {
			continue
		}
		if status == "fail" && row.Status < 400 {
			continue
		}
		if mdl != "" && row.Model != mdl {
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
	total := len(h.St.Data.Logs)
	h.St.Mu.RUnlock()
	write(w, 200, map[string]any{"total": total, "rows": rows})
}

func (h *Handler) usageRoute(w http.ResponseWriter, r *http.Request) {
	hours := 24
	if v, err := strconv.Atoi(r.URL.Query().Get("hours")); err == nil && v > 0 {
		hours = v
	}
	h.St.Mu.RLock()
	keys := make([]string, 0, len(h.St.Data.Usage))
	for k := range h.St.Data.Usage {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > hours {
		keys = keys[len(keys)-hours:]
	}
	out := []map[string]any{}
	for _, k := range keys {
		b := h.St.Data.Usage[k]
		out = append(out, map[string]any{"hour": k, "req": b.Req, "ok": b.OK, "fail": b.Fail,
			"pt": b.PT, "ct": b.CT, "p50": model.Percentile(b.Lat, 50), "p95": model.Percentile(b.Lat, 95), "models": b.Models})
	}
	h.St.Mu.RUnlock()
	write(w, 200, map[string]any{"hours": out})
}

func (h *Handler) exportRoute(w http.ResponseWriter) {
	h.St.Mu.RLock()
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	acs := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		acs = append(acs, map[string]any{"id": a.ID, "label": a.Label, "userId": a.UserID, "name": a.Name,
			"cookie": "", "enabled": a.Enabled, "lastGrantDate": a.LastGrantDate,
			"totalGranted": a.TotalGranted, "grantCount": a.GrantCount})
	}
	out := map[string]any{"version": h.St.Data.Version, "config": cfg, "keys": h.St.Data.Keys,
		"clientKeys": h.St.Data.ClientKeys, "accounts": acs, "logs": h.St.Data.Logs, "usage": h.St.Data.Usage}
	h.St.Mu.RUnlock()
	write(w, 200, out)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
