// Package gateway 提供 OpenAI 兼容的 /v1 接口。
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type Handler struct {
	St     *store.Store
	Picker *pool.Picker
	Up     *upstream.Client
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg, typ, code string) {
	e := map[string]any{"message": msg, "type": typ}
	if code != "" {
		e["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func card(id, owner string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": owner}
}

type caller struct {
	Byok  string
	Label string
}

func (h *Handler) resolveCaller(r *http.Request) (caller, string) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return caller{}, "缺少 Authorization 头"
	}
	if strings.HasPrefix(tok, "meoo_project_") && len(tok) > 16 {
		return caller{Byok: tok, Label: "BYOK"}, ""
	}
	if strings.HasPrefix(tok, "meoo_ak_") {
		return caller{}, "个人 AK 不能调用模型网关，请用项目服务 AK（meoo_project_…）"
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	for _, c := range h.St.Data.ClientKeys {
		if c.Enabled && c.Key == tok {
			return caller{Label: c.Label}, ""
		}
	}
	if len(h.St.Data.Keys) == 0 && len(h.St.Data.ClientKeys) == 0 {
		return caller{}, "服务未配置 key（控制台添加或使用 BYOK）"
	}
	return caller{}, "无效的 API key"
}

func (h *Handler) resolveModel(m string) string {
	if m == "" {
		return "qwen3.8-flash"
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	if v, ok := h.St.Data.Config.Aliases[strings.ToLower(strings.TrimSpace(m))]; ok {
		return v
	}
	return strings.TrimSpace(m)
}

func (h *Handler) knownModel(id string) bool {
	for _, m := range model.NativeModels {
		if m == id {
			return true
		}
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	_, ok := h.St.Data.Config.Aliases[strings.ToLower(id)]
	return ok
}

func (h *Handler) hidden(id string) bool {
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	for _, m := range h.St.Data.Config.HiddenModels {
		if m == id {
			return true
		}
	}
	return false
}

func (h *Handler) shape(body map[string]any, resolved string) map[string]any {
	out := map[string]any{}
	for k, v := range body {
		out[k] = v
	}
	out["model"] = resolved
	if _, ok := out["messages"]; !ok {
		out["messages"] = []any{}
	}
	if v, ok := out["max_completion_tokens"]; ok {
		if _, has := out["max_tokens"]; !has {
			out["max_tokens"] = v
		}
		delete(out, "max_completion_tokens")
	}
	h.St.Mu.RLock()
	def := h.St.Data.Config.DefaultMaxTokens
	h.St.Mu.RUnlock()
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = def
	}
	if stream, _ := out["stream"].(bool); !stream {
		delete(out, "stream_options")
	} else if so, ok := out["stream_options"].(map[string]any); ok {
		so["include_usage"] = true
	}
	out["n"] = 1
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/v1/models" || path == "/models":
		h.listModels(w, r)
	case strings.HasPrefix(path, "/v1/models/"):
		h.getModel(w, r, strings.TrimPrefix(path, "/v1/models/"))
	case path == "/v1/chat/completions" || path == "/chat/completions":
		h.chat(w, r)
	case path == "/v1/completions" || path == "/completions":
		h.legacy(w, r)
	case path == "/v1/responses" || path == "/responses":
		h.responses(w, r)
	case path == "/v1/embeddings" || path == "/embeddings" || path == "/v1/audio/speech" ||
		path == "/v1/images/generations" || path == "/v1/moderations":
		fail(w, 404, "上游不提供 "+path, "invalid_request_error", "unsupported_endpoint")
	default:
		fail(w, 404, "未知路径 "+r.Method+" "+path, "invalid_request_error", "unknown_url")
	}
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if _, msg := h.resolveCaller(r); msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	data := []any{}
	for _, m := range model.NativeModels {
		if !h.hidden(m) {
			data = append(data, card(m, "meoo"))
		}
	}
	h.St.Mu.RLock()
	for a := range h.St.Data.Config.Aliases {
		if !h.hidden(a) {
			data = append(data, card(a, "meoo-alias"))
		}
	}
	h.St.Mu.RUnlock()
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request, id string) {
	if _, msg := h.resolveCaller(r); msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	if !h.knownModel(id) || h.hidden(id) {
		fail(w, 404, "模型 "+id+" 不存在", "invalid_request_error", "model_not_found")
		return
	}
	owner, out := "meoo", id
	h.St.Mu.RLock()
	if v, ok := h.St.Data.Config.Aliases[strings.ToLower(id)]; ok {
		owner, out = "meoo-alias", v
	}
	h.St.Mu.RUnlock()
	writeJSON(w, 200, card(out, owner))
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		fail(w, 405, "只支持 POST", "invalid_request_error", "method_not_allowed")
		return
	}
	var body map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	if msgs, ok := body["messages"].([]any); !ok || len(msgs) == 0 {
		fail(w, 400, "'messages' is a required property", "invalid_request_error", "missing_messages")
		return
	}
	c, msg := h.resolveCaller(r)
	if msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	resolved := h.resolveModel(fmt.Sprint(body["model"]))
	n := 1
	if v, ok := body["n"].(float64); ok && v > 1 {
		n = int(v)
		if n > 4 {
			n = 4
		}
	}
	payload := h.shape(body, resolved)
	payload["n"] = n

	keyStr := c.Byok
	keyLabel := c.Label
	var picked *model.Key
	if keyStr == "" {
		k := h.Picker.Pick()
		if k == nil {
			h.St.Record(&model.LogRow{Method: r.Method, Path: r.URL.Path, Status: 503, KeyLabel: keyLabel})
			fail(w, 503, "当前没有可用 key（全部禁用或在冷却中）", "api_error", "no_available_key")
			return
		}
		keyStr, keyLabel, picked = k.Key, k.Label, k
	}
	start := time.Now()
	stream, _ := payload["stream"].(bool)

	if n > 1 {
		if stream {
			fail(w, 400, "n>1 不支持 stream=true", "invalid_request_error", "invalid_parameter_error")
			return
		}
		h.chatN(w, r, c, keyStr, keyLabel, resolved, payload, n, start, picked)
		return
	}

	resp, err := h.Up.Chat(r.Context(), payload, keyStr, h.timeout(), stream)
	if err != nil {
		status, typ, m := 502, "api_error", err.Error()
		if ue, ok := err.(*upstream.Error); ok {
			status, typ, m = ue.Status, ue.Type, ue.Message
		}
		h.Picker.MarkKey(picked, false, status == 429 || status >= 500)
		h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: status,
			Ms: time.Since(start).Milliseconds(), Model: resolved, KeyLabel: keyLabel, Byok: c.Byok != "", Error: m})
		fail(w, status, m, typ, "")
		return
	}
	defer resp.Body.Close()
	h.Picker.MarkKey(picked, true, false)

	if !stream {
		raw, _ := io.ReadAll(resp.Body)
		var d map[string]any
		if json.Unmarshal(raw, &d) != nil {
			fail(w, 502, "上游返回非 JSON", "api_error", "")
			return
		}
		if _, ok := d["model"]; !ok {
			d["model"] = resolved
		}
		var pt, ct int64
		if u, ok := d["usage"].(map[string]any); ok {
			if v, ok := u["prompt_tokens"].(float64); ok {
				pt = int64(v)
			}
			if v, ok := u["completion_tokens"].(float64); ok {
				ct = int64(v)
			}
		}
		h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: 200,
			Ms: time.Since(start).Milliseconds(), Model: resolved, PT: pt, CT: ct,
			KeyLabel: keyLabel, Byok: c.Byok != ""})
		writeJSON(w, 200, d)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 8192)
	for {
		nr, rerr := resp.Body.Read(buf)
		if nr > 0 {
			if _, werr := w.Write(buf[:nr]); werr != nil {
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			break
		}
	}
	h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: 200,
		Ms: time.Since(start).Milliseconds(), Model: resolved, KeyLabel: keyLabel, Byok: c.Byok != ""})
}

func (h *Handler) timeout() time.Duration {
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	return time.Duration(h.St.Data.Config.TimeoutMs) * time.Millisecond
}

func (h *Handler) chatN(w http.ResponseWriter, r *http.Request, c caller, keyStr, keyLabel, resolved string, payload map[string]any, n int, start time.Time, picked *model.Key) {
	single := map[string]any{}
	for k, v := range payload {
		single[k] = v
	}
	single["n"] = 1
	type item struct {
		data map[string]any
		err  error
	}
	ch := make(chan item, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := h.Up.Chat(r.Context(), single, keyStr, h.timeout(), false)
			if err != nil {
				ch <- item{err: err}
				return
			}
			defer resp.Body.Close()
			var d map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&d)
			ch <- item{data: d}
		}()
	}
	merged := map[string]any{}
	choices := []any{}
	for i := 0; i < n; i++ {
		it := <-ch
		if it.err != nil {
			h.Picker.MarkKey(picked, false, true)
			fail(w, 502, it.err.Error(), "api_error", "")
			return
		}
		for k, v := range it.data {
			merged[k] = v
		}
		if cs, ok := it.data["choices"].([]any); ok {
			for _, ch1 := range cs {
				if m, ok := ch1.(map[string]any); ok {
					m["index"] = i
					choices = append(choices, m)
				}
			}
		}
	}
	merged["choices"] = choices
	merged["model"] = resolved
	h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: 200,
		Ms: time.Since(start).Milliseconds(), Model: resolved, KeyLabel: keyLabel, Byok: c.Byok != ""})
	writeJSON(w, 200, merged)
}

func rid(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return model.RandID("", 8)
}
