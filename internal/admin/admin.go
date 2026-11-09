// Package admin 提供控制台 API：账号（含所属 API Key）、签到、日志与用量。
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

type importTokens struct {
	mu sync.Mutex
	m  map[string]int64
}

func NewImportTokens() *importTokens { return &importTokens{m: map[string]int64{}} }

func (t *importTokens) issue() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	t.m[tok] = time.Now().Add(10 * time.Minute).UnixMilli()
	t.mu.Unlock()
	return tok
}

func (t *importTokens) use(tok string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.m[tok]
	if !ok || exp < time.Now().UnixMilli() {
		delete(t.m, tok)
		return false
	}
	delete(t.m, tok)
	return true
}

type Handler struct {
	St     *store.Store
	Picker *pool.Picker
	Acc    *account.Service
	Tokens *Tokens
	Import *importTokens
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
	if path == "/admin/api/sso/import" && r.Method == http.MethodPost {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !h.Import.use(tok) {
			write(w, 401, map[string]any{"error": "导入令牌无效或已使用"})
			return true
		}
		var in struct{ Cookie string }
		readBody(r, &in)
		if strings.TrimSpace(in.Cookie) == "" {
			write(w, 400, map[string]any{"error": "缺少 Cookie"})
			return true
		}
		a, out, err := h.Acc.Add(strings.TrimSpace(in.Cookie))
		resp := map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name}
		for k, v := range out {
			resp[k] = v
		}
		if err != nil {
			resp["error"] = err.Error()
			write(w, 400, resp)
			return true
		}
		write(w, 200, resp)
		return true
	}
	if !h.Tokens.valid(r) {
		write(w, 401, map[string]any{"error": "unauthorized"})
		return true
	}
	h.route(w, r, path)
	return true
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/admin/api/state" && r.Method == http.MethodGet:
		h.state(w)
	case path == "/admin/api/accounts":
		h.accountsRoot(w, r)
	case strings.HasPrefix(path, "/admin/api/accounts/"):
		h.accountRoute(w, r, strings.TrimPrefix(path, "/admin/api/accounts/"))
	case path == "/admin/api/accounts-checkin-all" && r.Method == http.MethodPost:
		write(w, 200, map[string]any{"results": checkin.RunAll(h.St, true)})
	case path == "/admin/api/sso/url" && r.Method == http.MethodGet:
		u, err := h.Acc.SSOURL()
		if err != nil {
			write(w, 502, map[string]any{"error": err.Error()})
			return
		}
		write(w, 200, map[string]any{"url": u})
	case path == "/admin/api/sso/token" && r.Method == http.MethodPost:
		tok := h.Import.issue()
		write(w, 200, map[string]any{"token": tok, "expiresIn": 600})
	case path == "/admin/api/api-keys":
		h.apiKeysRoot(w, r)
	case strings.HasPrefix(path, "/admin/api/api-keys/"):
		h.apiKeyRoute(w, r, strings.TrimPrefix(path, "/admin/api/api-keys/"))
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

func (h *Handler) state(w http.ResponseWriter) {
	now := time.Now().UnixMilli()
	h.St.Mu.RLock()
	out := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		acc := map[string]any{
			"id": a.ID, "userId": a.UserID, "name": a.Name, "cookie": maskCookie(a.Cookie),
			"enabled": a.Enabled, "expired": a.Expired, "pingOk": a.PingOK, "lastPing": a.LastPing,
			"lastGrantDate": a.LastGrantDate, "totalGranted": a.TotalGranted, "grantCount": a.GrantCount,
			"last": a.Last, "createdAt": a.CreatedAt, "points": a.Points, "pointsAt": a.PointsAt,
		}
		if k := findKey(h.St.Data.Keys, a.KeyID); k != nil {
			acc["key"] = map[string]any{
				"id": k.ID, "key": model.Mask(k.Key), "health": k.Health.State,
				"latencyMs": k.Health.LatencyMs, "cooldown": max64(0, (k.CooldownUntil-now)/1000),
				"stats": k.Stats,
			}
		}
		out = append(out, acc)
	}
	orphans := []map[string]any{}
	for _, k := range h.St.Data.Keys {
		if k.AccountID != "" {
			continue
		}
		orphans = append(orphans, map[string]any{
			"id": k.ID, "key": model.Mask(k.Key), "health": k.Health.State,
			"latencyMs": k.Health.LatencyMs, "cooldown": max64(0, (k.CooldownUntil-now)/1000),
		})
	}
	aks := []map[string]any{}
	for _, c := range h.St.Data.ClientKeys {
		kv := model.Mask(c.Key)
		aks = append(aks, map[string]any{"id": c.ID, "key": kv, "enabled": c.Enabled,
			"createdAt": c.CreatedAt, "stats": c.Stats})
	}
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	body := map[string]any{
		"config": cfg, "accounts": out, "orphanKeys": orphans, "apiKeys": aks, "models": model.NativeModels,
		"alerts": h.St.AlertsLocked(), "lastAutoRun": h.St.Data.LastAutoRun,
		"uptime": time.Since(h.Start).Seconds(),
	}
	h.St.Mu.RUnlock()
	write(w, 200, body)
}

func maskCookie(c string) string {
	if len(c) <= 12 {
		return c
	}
	return c[:8] + "…(" + strconv.Itoa(len(c)) + ")"
}

func findKey(keys []*model.Key, id string) *model.Key {
	for _, k := range keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func (h *Handler) accountsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.St.Mu.RLock()
		list := []map[string]any{}
		for _, a := range h.St.Data.Accounts {
			list = append(list, map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name,
				"enabled": a.Enabled, "expired": a.Expired, "keyId": a.KeyID})
		}
		h.St.Mu.RUnlock()
		write(w, 200, map[string]any{"accounts": list})
		return
	}
	var in struct{ Cookie string }
	readBody(r, &in)
	if strings.TrimSpace(in.Cookie) == "" {
		write(w, 400, map[string]any{"error": "缺少 Cookie"})
		return
	}
	a, out, err := h.Acc.Add(strings.TrimSpace(in.Cookie))
	resp := map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name}
	for k, v := range out {
		resp[k] = v
	}
	if err != nil {
		resp["error"] = err.Error()
		write(w, 400, resp)
		return
	}
	write(w, 200, resp)
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
	case sub == "/points" && r.Method == http.MethodPost:
		v, err := h.Acc.Points(a)
		if err != nil {
			write(w, 400, map[string]any{"error": err.Error()})
			return
		}
		_ = h.St.Save()
		write(w, 200, map[string]any{"points": v})
	case sub == "/reset-cooldown" && r.Method == http.MethodPost:
		h.St.Mu.Lock()
		if k := findKey(h.St.Data.Keys, a.KeyID); k != nil {
			k.CooldownUntil = 0
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	case sub == "/probe" && r.Method == http.MethodPost:
		h.St.Mu.RLock()
		k := findKey(h.St.Data.Keys, a.KeyID)
		h.St.Mu.RUnlock()
		if k == nil {
			write(w, 400, map[string]any{"error": "该账号还没有 API Key"})
			return
		}
		write(w, 200, h.Picker.Probe(k))
	case sub == "/key":
		h.St.Mu.RLock()
		k := findKey(h.St.Data.Keys, a.KeyID)
		cookie := a.Cookie
		h.St.Mu.RUnlock()
		if k == nil {
			write(w, 400, map[string]any{"error": "该账号还没有 API Key"})
			return
		}
		if r.URL.Query().Get("cookie") == "1" {
			write(w, 200, map[string]any{"key": k.Key, "cookie": cookie})
			return
		}
		write(w, 200, map[string]any{"key": k.Key})
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
	case r.Method == http.MethodPatch:
		var in map[string]any
		readBody(r, &in)
		h.St.Mu.Lock()
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
		if a.KeyID != "" {
			keys := h.St.Data.Keys[:0]
			for _, k := range h.St.Data.Keys {
				if k.ID != a.KeyID {
					keys = append(keys, k)
				}
			}
			h.St.Data.Keys = keys
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	default:
		write(w, 404, map[string]any{"error": "unknown"})
	}
}

func (h *Handler) apiKeysRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.St.Mu.RLock()
		list := []map[string]any{}
		for _, c := range h.St.Data.ClientKeys {
			list = append(list, map[string]any{"id": c.ID, "key": model.Mask(c.Key), "enabled": c.Enabled,
				"createdAt": c.CreatedAt, "stats": c.Stats})
		}
		h.St.Mu.RUnlock()
		write(w, 200, map[string]any{"apiKeys": list})
		return
	}
	c := store.NewClientKey("", "")
	h.St.Mu.Lock()
	h.St.Data.ClientKeys = append(h.St.Data.ClientKeys, c)
	h.St.Mu.Unlock()
	_ = h.St.Save()
	write(w, 200, map[string]any{"id": c.ID, "key": c.Key})
}

func (h *Handler) apiKeyRoute(w http.ResponseWriter, r *http.Request, rest string) {
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
	switch {
	case sub == "/reveal":
		write(w, 200, map[string]any{"key": c.Key})
	case r.Method == http.MethodPatch:
		var in map[string]any
		readBody(r, &in)
		h.St.Mu.Lock()
		if v, ok := in["enabled"].(bool); ok {
			c.Enabled = v
		}
		h.St.Mu.Unlock()
		_ = h.St.Save()
		write(w, 200, map[string]any{"ok": true})
	case r.Method == http.MethodDelete:
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
	default:
		write(w, 404, map[string]any{"error": "unknown"})
	}
}

func (h *Handler) configRoute(w http.ResponseWriter, r *http.Request) {
	var in map[string]any
	readBody(r, &in)
	h.St.Mu.Lock()
	num := func(k string, dst *int) {
		if v, ok := in[k].(float64); ok && v >= 0 {
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
	aks := []map[string]any{}
	for _, c := range h.St.Data.ClientKeys {
		kv := model.Mask(c.Key)
		aks = append(aks, map[string]any{"id": c.ID, "key": kv, "enabled": c.Enabled,
			"createdAt": c.CreatedAt, "stats": c.Stats})
	}
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	acs := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		acs = append(acs, map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name, "cookie": "",
			"enabled": a.Enabled, "lastGrantDate": a.LastGrantDate, "totalGranted": a.TotalGranted})
	}
	out := map[string]any{"version": h.St.Data.Version, "config": cfg, "accounts": acs,
		"keys": h.St.Data.Keys, "logs": h.St.Data.Logs, "usage": h.St.Data.Usage}
	h.St.Mu.RUnlock()
	write(w, 200, out)
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
