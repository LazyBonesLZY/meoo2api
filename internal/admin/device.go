package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"meoo2api/internal/account"
)

type deviceSession struct {
	owner   string
	expires time.Time
	ctx     context.Context
	cancel  context.CancelFunc
	gate    chan struct{}
	last    time.Time
	result  []byte
}

func (h *Handler) forgetDevices(owners ...string) {
	h.deviceMu.Lock()
	defer h.deviceMu.Unlock()
	for owner, s := range h.pendingDevices {
		if len(owners) == 0 || owner == owners[0] {
			s.cancel()
			delete(h.pendingDevices, owner)
		}
	}
	for sid, s := range h.devices {
		if len(owners) == 0 || s.owner == owners[0] {
			s.cancel()
			delete(h.devices, sid)
		}
	}
}

func (h *Handler) deviceStart(w http.ResponseWriter, r *http.Request) {
	owner := bearer(r)
	if !h.Tokens.allow("device:"+owner, 5) {
		rateLimited(w)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	session := &deviceSession{owner: owner, expires: time.Now().Add(10 * time.Minute), ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	h.deviceMu.Lock()
	if h.pendingDevices == nil {
		h.pendingDevices = make(map[string]*deviceSession)
	}
	if pending := h.pendingDevices[owner]; pending != nil {
		h.deviceMu.Unlock()
		cancel()
		rateLimited(w)
		return
	}
	h.pendingDevices[owner] = session
	h.deviceMu.Unlock()
	defer func() {
		h.deviceMu.Lock()
		if h.pendingDevices[owner] == session {
			delete(h.pendingDevices, owner)
		}
		h.deviceMu.Unlock()
	}()
	startCtx, startCancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(ctx, startCancel)
	defer startCancel()
	defer stop()
	sid, target, err := h.Acc.StartDeviceContext(startCtx)
	if err != nil {
		cancel()
		write(w, 502, map[string]any{"error": "创建授权会话失败，请稍后重试"})
		return
	}
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host != "meoo.com" || u.Path != "/cli/login" || u.User != nil || u.Fragment != "" || sid == "" || len(sid) > 256 || strings.ContainsAny(sid, "/\\\r\n?#") {
		cancel()
		write(w, 502, map[string]any{"error": "授权会话响应无效"})
		return
	}
	h.deviceMu.Lock()
	if h.pendingDevices[owner] != session || startCtx.Err() != nil {
		h.deviceMu.Unlock()
		cancel()
		write(w, 408, map[string]any{"error": "授权创建已取消"})
		return
	}
	if h.devices == nil {
		h.devices = make(map[string]*deviceSession)
	}
	now := time.Now()
	for id, s := range h.devices {
		if !now.Before(s.expires) || s.owner == owner {
			s.cancel()
			delete(h.devices, id)
		}
	}
	if len(h.devices) >= 256 {
		h.deviceMu.Unlock()
		cancel()
		rateLimited(w)
		return
	}
	h.devices[sid] = session
	delete(h.pendingDevices, owner)
	h.deviceMu.Unlock()
	if !h.Tokens.valid(r) {
		h.forgetDevices(owner)
		write(w, 401, map[string]any{"error": "登录已过期"})
		return
	}
	write(w, 200, map[string]any{"sessionId": sid, "url": target})
}

func (h *Handler) devicePoll(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	if sid == "" || len(sid) > 256 {
		write(w, 400, map[string]any{"error": "sid 无效"})
		return
	}
	h.deviceMu.Lock()
	s := h.devices[sid]
	if s == nil || s.owner != bearer(r) || !time.Now().Before(s.expires) {
		h.deviceMu.Unlock()
		write(w, 404, map[string]any{"error": "授权会话不存在或已过期，请重新开始"})
		return
	}
	h.deviceMu.Unlock()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		write(w, 408, map[string]any{"error": "授权查询已取消"})
		return
	}
	if ctx.Err() != nil {
		write(w, 408, map[string]any{"error": "授权查询已取消"})
		return
	}
	if s.result != nil {
		writeJSON(w, 200, s.result)
		return
	}
	if time.Since(s.last) < 2*time.Second {
		write(w, 200, map[string]any{"status": "pending"})
		return
	}
	s.last = time.Now()
	st, err := h.Acc.PollDeviceContext(ctx, sid)
	if err != nil || st == nil {
		write(w, 502, map[string]any{"error": "查询授权失败，请稍后重试"})
		return
	}
	if st.Status != "approved" {
		status := st.Status
		if status != "pending" && status != "expired" && status != "denied" {
			status = "expired"
		}
		if status != "pending" {
			s.result, _ = json.Marshal(map[string]any{"status": status})
		}
		write(w, 200, map[string]any{"status": status})
		return
	}
	a, out, err := h.Acc.AddFromDeviceContext(ctx, st)
	if a == nil {
		// Keep the consumed credential in account.Service's private cache and
		// retry identity/onboarding on the next poll; never cache a false success.
		write(w, 502, map[string]any{"error": "授权身份校验暂未完成，可重试查询；重复失败请重新授权"})
		return
	}
	resp := map[string]any{"status": "approved"}
	if a != nil {
		h.St.Mu.RLock()
		resp["id"], resp["userId"], resp["name"] = a.ID, a.UserID, a.Name
		h.St.Mu.RUnlock()
	}
	copyAccountOutput(resp, out)
	if err != nil {
		resp["error"] = accountError(err, out)
	}
	// approved is consumable upstream; cache the final response even on partial
	// onboarding failure so concurrent/retried polls never consume it twice.
	s.result, _ = json.Marshal(resp)
	writeJSON(w, 200, s.result)
}

func accountError(err error, out map[string]any) string {
	if err == nil {
		return ""
	}
	var business *account.BusinessError
	if errors.As(err, &business) {
		// Translate a known business condition rather than displaying untrusted
		// upstream text, which may include echoed credentials or request details.
		switch business.Code {
		case "QUOTA_EXCEEDED":
			return "Meoo 云服务实例数已达上限，释放已有实例后重试"
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "接入已取消或超时；已保存账号可点击重试接入"
	}
	for _, stage := range []struct{ needle, message string }{
		{"个人 AK", "个人凭证准备失败，请重新登录账号后重试"},
		{"建项目", "创建项目失败；已保存账号可点击重试接入"},
		{"云服务", "云服务开通失败；已保存项目，下次将复用并重试"},
		{"项目凭证", "读取项目凭证失败；请点击重试接入"},
		{"识别", "账号身份校验失败，请重新登录"},
		{"身份", "账号身份校验失败，请重新登录"},
		{"持久化", "账号已更新但持久化失败，请检查数据目录"},
	} {
		if strings.Contains(err.Error(), stage.needle) {
			return stage.message
		}
	}
	if out["stage"] == "cloud" {
		return "云服务开通未完成，请点击重试接入"
	}
	return "账号接入未完成；已保存账号可点击重试接入，未保存时请重新登录"
}

func copyAccountOutput(dst, src map[string]any) {
	for _, name := range []string{"points", "session", "project", "keyId", "stage"} {
		if v, ok := src[name]; ok {
			dst[name] = v
		}
	}
	if key, ok := src["key"].(string); ok {
		dst["key"] = maskSecret(key)
	}
	if _, ok := src["error"]; ok {
		dst["error"] = "账号接入未完成，请稍后重试"
	}
}
