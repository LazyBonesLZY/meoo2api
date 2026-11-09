// Package admin provides the five-page management console API.
package admin

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/catalog"
	"meoo2api/internal/checkin"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
)

type Handler struct {
	St      *store.Store
	Picker  *pool.Picker
	Acc     *account.Service
	Tokens  *Tokens
	Start   time.Time
	Catalog *catalog.Service

	deviceMu       sync.Mutex
	devices        map[string]*deviceSession
	pendingDevices map[string]*deviceSession
	configMu       sync.Mutex
}

func write(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status, body = http.StatusInternalServerError, []byte(`{"error":"响应编码失败"}`)
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Snapshot shared objects to JSON while the store read lock is held. Never hold
// the lock while writing to a potentially slow network client.
func (h *Handler) writeLocked(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	h.St.Mu.RUnlock()
	if err != nil {
		write(w, 500, map[string]any{"error": "响应编码失败"})
		return
	}
	writeJSON(w, 200, body)
}

func method(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for _, m := range allowed {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	write(w, 405, map[string]any{"error": "请求方法不支持"})
	return false
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) bool {
	path := strings.TrimRight(r.URL.Path, "/")
	if !strings.HasPrefix(path, "/admin/api/") {
		return false
	}
	for _, header := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Headers", "Access-Control-Allow-Methods", "Access-Control-Allow-Credentials"} {
		w.Header().Del(header)
	}
	w.Header().Set("Cache-Control", "no-store")
	if !sameOrigin(r) {
		write(w, 403, map[string]any{"error": "仅支持同源控制台请求"})
		return true
	}
	if path == "/admin/api/login" {
		if method(w, r, http.MethodPost) {
			h.login(w, r)
		}
		return true
	}
	if !h.Tokens.valid(r) {
		write(w, 401, map[string]any{"error": "登录已过期，请重新登录"})
		return true
	}
	if !h.Tokens.allow("api:"+bearer(r), 600) {
		rateLimited(w)
		return true
	}
	if path == "/admin/api/logout" {
		if method(w, r, http.MethodPost) {
			h.Tokens.revoke(r)
			h.forgetDevices(bearer(r))
			write(w, 200, map[string]any{"ok": true})
		}
		return true
	}
	h.route(w, r, path)
	return true
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	// Key by actual peer address, never spoofable X-Forwarded-For.
	if !h.Tokens.allow("login:"+clientIP(r), 10) || !h.Tokens.allow("login:global", 100) {
		rateLimited(w)
		return
	}
	var in struct{ Username, Password string }
	if !readBody(w, r, &in) {
		return
	}
	if in.Username == "" {
		in.Username = "admin"
	}
	if len(in.Username) > 128 || len(in.Password) > 1024 {
		write(w, 400, map[string]any{"error": "用户名或密码过长"})
		return
	}
	h.St.Mu.RLock()
	user, hash := h.St.Data.Config.AdminUser, h.St.Data.Config.AdminPass
	h.St.Mu.RUnlock()
	valid := model.VerifyPassword(hash, in.Password)
	if !valid || in.Username != user {
		write(w, 401, map[string]any{"ok": false, "error": "用户名或密码错误"})
		return
	}
	// Serialize verification/issuance with password changes so a request that
	// verified the old hash cannot mint a new session after the reset.
	upgrade := ""
	if len(hash) == 64 {
		upgrade = model.HashPassword(in.Password)
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()
	h.St.Mu.RLock()
	current := h.St.Data.Config.AdminPass == hash && h.St.Data.Config.AdminUser == user
	h.St.Mu.RUnlock()
	if !current {
		write(w, 401, map[string]any{"error": "登录凭据已更新，请重新登录"})
		return
	}
	if upgrade != "" {
		h.St.Mu.Lock()
		h.St.Data.Config.AdminPass = upgrade
		h.St.Mu.Unlock()
		if !h.saved(w) {
			return
		}
	}
	tok, err := h.Tokens.issue()
	if err != nil {
		write(w, 500, map[string]any{"error": "创建会话失败"})
		return
	}
	write(w, 200, map[string]any{"ok": true, "token": tok})
}

func (h *Handler) route(w http.ResponseWriter, r *http.Request, path string) {
	switch {
	case path == "/admin/api/state":
		if method(w, r, "GET") {
			h.state(w, r)
		}
	case path == "/admin/api/accounts":
		h.accountsRoot(w, r)
	case strings.HasPrefix(path, "/admin/api/accounts/"):
		h.accountRoute(w, r, strings.TrimPrefix(path, "/admin/api/accounts/"))
	case path == "/admin/api/accounts-checkin-all":
		if method(w, r, "POST") {
			results := checkin.RunAllContext(r.Context(), h.St, true)
			for i := range results {
				if results[i].Error != "" {
					results[i].Error = "签到失败，请检查账号状态"
				}
			}
			if h.saved(w) {
				write(w, 200, map[string]any{"results": results})
			}
		}
	case path == "/admin/api/device/start":
		if method(w, r, "POST") {
			h.deviceStart(w, r)
		}
	case path == "/admin/api/device/poll":
		if method(w, r, "GET") {
			h.devicePoll(w, r)
		}
	case path == "/admin/api/device/cancel":
		if method(w, r, "POST") {
			h.forgetDevices(bearer(r))
			write(w, 200, map[string]any{"ok": true})
		}
	case path == "/admin/api/api-keys":
		h.apiKeysRoot(w, r)
	case strings.HasPrefix(path, "/admin/api/api-keys/"):
		h.apiKeyRoute(w, r, strings.TrimPrefix(path, "/admin/api/api-keys/"))
	case strings.HasPrefix(path, "/admin/api/orphan-keys/"):
		h.orphanKeyRoute(w, r, strings.TrimPrefix(path, "/admin/api/orphan-keys/"))
	case path == "/admin/api/config":
		if method(w, r, "PATCH") {
			h.configRoute(w, r)
		}
	case path == "/admin/api/logs":
		if method(w, r, "GET") {
			h.logsRoute(w, r)
		}
	case path == "/admin/api/usage":
		if method(w, r, "GET") {
			h.usageRoute(w, r)
		}
	case path == "/admin/api/export":
		if method(w, r, "GET") {
			h.exportRoute(w)
		}
	default:
		write(w, 404, map[string]any{"error": "unknown admin path"})
	}
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 18 {
		return "••••"
	}
	return model.Mask(s)
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	snapshot := catalog.Builtin()
	if h.Catalog != nil {
		snapshot = h.Catalog.Get(r.Context(), "")
	}
	modelIDs := make([]string, 0, len(snapshot.Data))
	for _, entry := range snapshot.Data {
		modelIDs = append(modelIDs, entry.ID)
	}
	now := time.Now().UnixMilli()
	h.St.Mu.RLock()
	out := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		acc := map[string]any{
			"id": a.ID, "userId": a.UserID, "name": a.Name, "cookie": maskCookie(a.Cookie), "hasCookie": a.Cookie != "",
			"enabled": a.Enabled, "expired": a.Expired, "pingOk": a.PingOK, "lastPing": a.LastPing,
			"lastGrantDate": a.LastGrantDate, "totalGranted": a.TotalGranted, "grantCount": a.GrantCount,
			"createdAt": a.CreatedAt, "points": a.Points, "pointsAt": a.PointsAt,
		}
		if a.Last != nil {
			last := *a.Last
			last.Error = "" // Upstream errors can embed credentials; only expose outcome.
			acc["last"] = last
		}
		if k := findKey(h.St.Data.Keys, a.KeyID); k != nil {
			acc["key"] = keySummary(k, now)
		}
		out = append(out, acc)
	}
	orphans := []map[string]any{}
	for _, k := range h.St.Data.Keys {
		if k.AccountID == "" {
			orphans = append(orphans, keySummary(k, now))
		}
	}
	usable := 0
	for _, k := range h.St.Data.Keys {
		if h.St.UsableKeyLocked(k, now) {
			usable++
		}
	}
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	h.writeLocked(w, map[string]any{
		"usableKeys": usable, "config": cfg, "accounts": out, "orphanKeys": orphans, "apiKeys": h.clientKeysLocked(), "models": modelIDs, "modelSync": map[string]any{"source": snapshot.Source, "updatedAt": snapshot.UpdatedAt, "stale": snapshot.Stale, "error": snapshot.Error, "defaultModel": snapshot.DefaultModel},
		"alerts": h.St.AlertsLocked(), "lastAutoRun": h.St.Data.LastAutoRun, "today": model.Today(),
		"uptime": time.Since(h.Start).Seconds(),
	})
}

func keySummary(k *model.Key, now int64) map[string]any {
	return map[string]any{"id": k.ID, "key": maskSecret(k.Key), "enabled": k.Enabled, "health": k.Health.State,
		"latencyMs": k.Health.LatencyMs, "cooldown": max64(0, (k.CooldownUntil-now)/1000), "stats": k.Stats}
}

func (h *Handler) clientKeysLocked() []map[string]any {
	out := []map[string]any{}
	for _, c := range h.St.Data.ClientKeys {
		out = append(out, map[string]any{"id": c.ID, "key": maskSecret(c.Key), "enabled": c.Enabled, "createdAt": c.CreatedAt, "stats": c.Stats})
	}
	return out
}

func maskCookie(c string) string {
	if c == "" {
		return ""
	}
	return "•••• (" + strconv.Itoa(len(c)) + ")"
}

func findKey(keys []*model.Key, id string) *model.Key {
	for _, k := range keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func (h *Handler) saved(w http.ResponseWriter) bool {
	if err := h.St.Save(); err != nil {
		h.St.Touch()
		write(w, 500, map[string]any{"error": "操作已应用，但持久化失败，请检查数据目录后重试"})
		return false
	}
	return true
}

func (h *Handler) accountsRoot(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET", "POST") {
		return
	}
	if r.Method == "GET" {
		h.St.Mu.RLock()
		list := []map[string]any{}
		for _, a := range h.St.Data.Accounts {
			list = append(list, map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name, "enabled": a.Enabled, "expired": a.Expired, "keyId": a.KeyID})
		}
		h.writeLocked(w, map[string]any{"accounts": list})
		return
	}
	var in struct{ Cookie string }
	if !readBody(w, r, &in) {
		return
	}
	in.Cookie = strings.TrimSpace(in.Cookie)
	if !validCookie(in.Cookie) {
		write(w, 400, map[string]any{"error": "Cookie 为空、过长或含换行"})
		return
	}
	a, out, err := h.Acc.AddContext(r.Context(), in.Cookie)
	h.accountResult(w, a, out, err)
}

func validCookie(cookie string) bool {
	return cookie != "" && len(cookie) <= 16384 && !strings.ContainsAny(cookie, "\r\n\x00")
}

func (h *Handler) accountResult(w http.ResponseWriter, a *model.Account, out map[string]any, err error) {
	resp := map[string]any{}
	if a != nil {
		h.St.Mu.RLock()
		resp["id"], resp["userId"], resp["name"] = a.ID, a.UserID, a.Name
		h.St.Mu.RUnlock()
	}
	copyAccountOutput(resp, out)
	if err != nil {
		resp["error"] = accountError(err, out)
		write(w, 400, resp)
		return
	}
	write(w, 200, resp)
}

func (h *Handler) accountRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || parts[0] == "" {
		write(w, 404, map[string]any{"error": "unknown"})
		return
	}
	id, sub := parts[0], ""
	if len(parts) == 2 {
		sub = parts[1]
	}
	a := h.Acc.Find(id)
	if a == nil {
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	if sub == "" {
		if !method(w, r, "PATCH", "DELETE") {
			return
		}
		if r.Method == "DELETE" {
			if err := h.Acc.DeleteContext(r.Context(), id); err != nil {
				write(w, 400, map[string]any{"error": "账号删除失败"})
				return
			}
		} else {
			var in struct {
				Enabled *bool   `json:"enabled"`
				Cookie  *string `json:"cookie"`
			}
			if !readBody(w, r, &in) {
				return
			}
			if in.Enabled == nil && in.Cookie == nil {
				write(w, 400, map[string]any{"error": "缺少变更字段"})
				return
			}
			if in.Cookie != nil {
				*in.Cookie = strings.TrimSpace(*in.Cookie)
				if *in.Cookie != "" && !validCookie(*in.Cookie) {
					write(w, 400, map[string]any{"error": "Cookie 无效"})
					return
				}
			}
			if err := h.Acc.UpdateContext(r.Context(), id, in.Enabled, in.Cookie); err != nil {
				write(w, 400, map[string]any{"error": "更新失败，请检查 Cookie 是否属于同一账号"})
				return
			}
		}
		write(w, 200, map[string]any{"ok": true})
		return
	}
	if sub == "key" {
		if !method(w, r, "GET") {
			return
		}
		h.St.Mu.RLock()
		k := findKey(h.St.Data.Keys, a.KeyID)
		if k == nil {
			h.St.Mu.RUnlock()
			write(w, 400, map[string]any{"error": "该账号还没有 API Key"})
			return
		}
		// Cookie is never revealed by this endpoint, including legacy ?cookie=1.
		h.writeLocked(w, map[string]any{"key": k.Key})
		return
	}
	if sub != "ping" && sub != "checkin" && sub != "points" && sub != "reset-cooldown" && sub != "probe" && sub != "onboard" {
		write(w, 404, map[string]any{"error": "unknown"})
		return
	}
	if !method(w, r, "POST") {
		return
	}
	switch sub {
	case "ping":
		ok, err := h.Acc.PingContext(r.Context(), a)
		snapshot := h.Acc.Snapshot(id)
		var lastPing int64
		if snapshot != nil {
			lastPing = snapshot.LastPing
		}
		msg := ""
		if err != nil {
			msg = "会话验证失败，请重新登录账号"
		}
		if h.saved(w) {
			write(w, 200, map[string]any{"ok": ok, "error": msg, "lastPing": lastPing})
		}
	case "checkin":
		out := checkin.RunContext(r.Context(), h.St, a)
		if out.Error != "" {
			out.Error = "签到失败，请检查账号状态"
		}
		if h.saved(w) {
			write(w, 200, out)
		}
	case "points":
		v, err := h.Acc.PointsContext(r.Context(), a)
		if err != nil {
			write(w, 400, map[string]any{"error": "查询积分失败，请检查账号状态"})
			return
		}
		if h.saved(w) {
			write(w, 200, map[string]any{"points": v})
		}
	case "reset-cooldown", "probe":
		h.St.Mu.Lock()
		k := findKey(h.St.Data.Keys, a.KeyID)
		if k != nil && sub == "reset-cooldown" {
			k.CooldownUntil = 0
		}
		h.St.Mu.Unlock()
		if k == nil {
			write(w, 400, map[string]any{"error": "该账号还没有 API Key"})
			return
		}
		if sub == "probe" {
			out := h.Picker.ProbeContext(r.Context(), k)
			if out.Error != "" {
				out.Error = "上游探测失败"
			}
			if h.saved(w) {
				write(w, 200, out)
			}
		} else if h.saved(w) {
			write(w, 200, map[string]any{"ok": true})
		}
	case "onboard":
		out, err := h.Acc.OnboardContext(r.Context(), a)
		h.accountResult(w, a, out, err)
	}
}

func (h *Handler) apiKeysRoot(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, "GET", "POST") {
		return
	}
	if r.Method == "GET" {
		h.St.Mu.RLock()
		h.writeLocked(w, map[string]any{"apiKeys": h.clientKeysLocked()})
		return
	}
	c := store.NewClientKey("", "")
	h.St.Mu.Lock()
	if len(h.St.Data.ClientKeys) >= 1000 {
		h.St.Mu.Unlock()
		write(w, 400, map[string]any{"error": "API Key 数量已达上限"})
		return
	}
	h.St.Data.ClientKeys = append(h.St.Data.ClientKeys, c)
	h.St.Mu.Unlock()
	if h.saved(w) {
		write(w, 200, map[string]any{"id": c.ID, "key": c.Key})
	}
}

func (h *Handler) apiKeyRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] != "reveal") {
		write(w, 404, map[string]any{"error": "unknown"})
		return
	}
	reveal := len(parts) == 2
	if reveal {
		if !method(w, r, "GET") {
			return
		}
	} else if !method(w, r, "PATCH", "DELETE") {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if r.Method == "PATCH" {
		if !readBody(w, r, &in) {
			return
		}
		if in.Enabled == nil {
			write(w, 400, map[string]any{"error": "缺少 enabled"})
			return
		}
	}
	h.St.Mu.Lock()
	index := -1
	for i, c := range h.St.Data.ClientKeys {
		if c.ID == parts[0] {
			index = i
			break
		}
	}
	if index == -1 {
		h.St.Mu.Unlock()
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	if reveal {
		key := h.St.Data.ClientKeys[index].Key
		h.St.Mu.Unlock()
		write(w, 200, map[string]any{"key": key})
		return
	}
	if r.Method == "PATCH" {
		h.St.Data.ClientKeys[index].Enabled = *in.Enabled
	} else {
		h.St.Data.ClientKeys = append(h.St.Data.ClientKeys[:index], h.St.Data.ClientKeys[index+1:]...)
	}
	h.St.Mu.Unlock()
	if h.saved(w) {
		write(w, 200, map[string]any{"ok": true})
	}
}

func (h *Handler) orphanKeyRoute(w http.ResponseWriter, r *http.Request, rest string) {
	parts := strings.Split(rest, "/")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] != "probe") {
		write(w, 404, map[string]any{"error": "not found"})
		return
	}
	probe := len(parts) == 2
	if probe {
		if !method(w, r, "POST") {
			return
		}
	} else if !method(w, r, "PATCH", "DELETE") {
		return
	}
	var in struct {
		Enabled *bool `json:"enabled"`
	}
	if r.Method == "PATCH" {
		if !readBody(w, r, &in) {
			return
		}
		if in.Enabled == nil {
			write(w, 400, map[string]any{"error": "缺少 enabled"})
			return
		}
	}
	h.St.Mu.Lock()
	index := -1
	for i, k := range h.St.Data.Keys {
		if k.ID != parts[0] {
			continue
		}
		linked := k.AccountID != ""
		for _, a := range h.St.Data.Accounts {
			if a.KeyID == k.ID {
				linked = true
			}
			for _, kid := range a.ProjectKeyIDs {
				if kid == k.ID {
					linked = true
				}
			}
		}
		if !linked {
			index = i
		}
		break
	}
	if index == -1 {
		h.St.Mu.Unlock()
		write(w, 404, map[string]any{"error": "未绑定 Key 不存在"})
		return
	}
	key := h.St.Data.Keys[index]
	if probe {
		h.St.Mu.Unlock()
		out := h.Picker.ProbeContext(r.Context(), key)
		if out.Error != "" {
			out.Error = "上游探测失败"
		}
		if h.saved(w) {
			write(w, 200, out)
		}
		return
	}
	if r.Method == "PATCH" {
		key.Enabled = *in.Enabled
	} else {
		h.St.Data.Keys = append(h.St.Data.Keys[:index], h.St.Data.Keys[index+1:]...)
	}
	h.St.Mu.Unlock()
	if h.saved(w) {
		write(w, 200, map[string]any{"ok": true})
	}
}

func (h *Handler) configRoute(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TimeoutMs        *int    `json:"timeoutMs"`
		DefaultMaxTokens *int    `json:"defaultMaxTokens"`
		CooldownSec      *int    `json:"cooldownSec"`
		HardCooldownSec  *int    `json:"hardCooldownSec"`
		MaxLogs          *int    `json:"maxLogs"`
		MaxUsageHours    *int    `json:"maxUsageHours"`
		CheckinHour      *int    `json:"checkinHour"`
		KeepAliveHours   *int    `json:"keepAliveHours"`
		Sticky           *bool   `json:"sticky"`
		AutoCheckin      *bool   `json:"autoCheckin"`
		AdminPassword    *string `json:"adminPassword"`
	}
	if !readBody(w, r, &in) {
		return
	}
	if in.AdminPassword != nil && (len(*in.AdminPassword) < 12 || len(*in.AdminPassword) > 1024) {
		write(w, 400, map[string]any{"error": "管理密码长度必须为 12 到 1024 字节"})
		return
	}
	passwordHash := ""
	if in.AdminPassword != nil {
		passwordHash = model.HashPassword(*in.AdminPassword)
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()
	// Password rotation may have revoked this session while its body/hash was
	// being processed. Do not let an already-revoked concurrent PATCH overwrite it.
	if !h.Tokens.valid(r) {
		write(w, 401, map[string]any{"error": "登录已过期，请重新登录"})
		return
	}
	h.St.Mu.Lock()
	cfg := h.St.Data.Config
	for _, p := range []struct {
		src *int
		dst *int
	}{
		{in.TimeoutMs, &cfg.TimeoutMs}, {in.DefaultMaxTokens, &cfg.DefaultMaxTokens}, {in.CooldownSec, &cfg.CooldownSec},
		{in.HardCooldownSec, &cfg.HardCooldownSec}, {in.MaxLogs, &cfg.MaxLogs}, {in.MaxUsageHours, &cfg.MaxUsageHours},
		{in.CheckinHour, &cfg.CheckinHour}, {in.KeepAliveHours, &cfg.KeepAliveHours},
	} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	if in.Sticky != nil {
		cfg.Sticky = *in.Sticky
	}
	if in.AutoCheckin != nil {
		cfg.AutoCheckin = *in.AutoCheckin
	}
	if passwordHash != "" {
		cfg.AdminPass = passwordHash
	}
	if err := cfg.Validate(); err != nil {
		h.St.Mu.Unlock()
		write(w, 400, map[string]any{"error": err.Error()})
		return
	}
	h.St.Data.Config = cfg
	h.St.Mu.Unlock()
	if passwordHash != "" {
		h.Tokens.Clear()
		h.forgetDevices()
	}
	if h.saved(w) {
		write(w, 200, map[string]any{"ok": true})
	}
}

func intQuery(w http.ResponseWriter, r *http.Request, name string, def, max int) (int, bool) {
	text := r.URL.Query().Get(name)
	if text == "" {
		return def, true
	}
	v, err := strconv.Atoi(text)
	if err != nil || v < 1 || v > max {
		write(w, 400, map[string]any{"error": name + " 参数超出范围"})
		return 0, false
	}
	return v, true
}

func (h *Handler) logsRoute(w http.ResponseWriter, r *http.Request) {
	limit, ok := intQuery(w, r, "limit", 100, 500)
	if !ok {
		return
	}
	status, mdl, q := r.URL.Query().Get("status"), r.URL.Query().Get("model"), r.URL.Query().Get("q")
	if (status != "" && status != "ok" && status != "fail") || len(mdl) > 128 || len(q) > 256 {
		write(w, 400, map[string]any{"error": "日志筛选参数无效"})
		return
	}
	h.St.Mu.RLock()
	rows := []model.LogRow{}
	for _, row := range h.St.Data.Logs {
		if (status == "ok" && row.Status >= 400) || (status == "fail" && row.Status < 400) || (mdl != "" && row.Model != mdl) {
			continue
		}
		if q != "" && !strings.Contains(row.Rid, q) && !strings.Contains(row.KeyLabel, q) {
			continue
		}
		safe := *row
		safe.Error = ""
		rows = append(rows, safe)
		if len(rows) >= limit {
			break
		}
	}
	h.writeLocked(w, map[string]any{"total": len(h.St.Data.Logs), "rows": rows})
}

func (h *Handler) usageRoute(w http.ResponseWriter, r *http.Request) {
	hours, ok := intQuery(w, r, "hours", 24, 8760)
	if !ok {
		return
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
		out = append(out, map[string]any{"hour": k, "req": b.Req, "ok": b.OK, "fail": b.Fail, "pt": b.PT, "ct": b.CT,
			"p50": model.Percentile(b.Lat, 50), "p95": model.Percentile(b.Lat, 95), "models": b.Models})
	}
	h.writeLocked(w, map[string]any{"hours": out})
}

func (h *Handler) exportRoute(w http.ResponseWriter) {
	h.St.Mu.RLock()
	cfg := h.St.Data.Config
	cfg.AdminPass = ""
	accounts := []map[string]any{}
	for _, a := range h.St.Data.Accounts {
		accounts = append(accounts, map[string]any{"id": a.ID, "userId": a.UserID, "name": a.Name, "enabled": a.Enabled,
			"lastGrantDate": a.LastGrantDate, "totalGranted": a.TotalGranted})
	}
	keys := []map[string]any{}
	for _, k := range h.St.Data.Keys {
		keys = append(keys, keySummary(k, time.Now().UnixMilli()))
	}
	// Deliberately omit credentials, raw upstream error messages, logs and usage
	// labels from the portable diagnostic export. This is not a restore backup.
	w.Header().Set("Content-Disposition", `attachment; filename="meoo2api-summary.json"`)
	h.writeLocked(w, map[string]any{"version": h.St.Data.Version, "config": cfg, "accounts": accounts,
		"keys": keys, "apiKeys": h.clientKeysLocked(), "redacted": true})
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
