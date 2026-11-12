// Package gateway 提供 OpenAI 兼容的 /v1 接口。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"meoo2api/internal/catalog"
	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

type Handler struct {
	St      *store.Store
	Picker  *pool.Picker
	Up      *upstream.Client
	Catalog *catalog.Service
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg, typ, code string) {
	e := map[string]any{"message": msg, "type": typ, "param": nil, "code": nil}
	if code != "" {
		e["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func card(id, owner string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": owner}
}

type caller struct {
	Byok      string
	Label     string
	ClientKey *model.ClientKey // private snapshot, never a shared state pointer
}

func (h *Handler) resolveCaller(r *http.Request) (caller, string) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return caller{}, "缺少有效的 Bearer Authorization 头"
	}
	tok := parts[1]
	if strings.HasPrefix(tok, "meoo_project_") && len(tok) > 16 {
		return caller{Byok: tok, Label: "BYOK"}, ""
	}
	if strings.HasPrefix(tok, "meoo_ak_") {
		return caller{}, "个人 AK 不支持模型网关，请用项目服务 AK（meoo_project_…）"
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	for _, c := range h.St.Data.ClientKeys {
		if c != nil && c.Enabled && c.Key == tok {
			cp := *c
			label := c.Label
			if label == "" {
				label = "api-key"
			}
			return caller{ClientKey: &cp, Label: label}, ""
		}
	}
	return caller{}, "无效的 API key"
}

func (h *Handler) resolveModel(m string) string {
	m = strings.TrimSpace(m)
	if m == "" {
		return "qwen3.8-flash"
	}
	h.St.Mu.RLock()
	defer h.St.Mu.RUnlock()
	if v, ok := h.St.Data.Config.Aliases[strings.ToLower(m)]; ok {
		return v
	}
	return m
}

func hiddenIn(id string, hidden []string) bool {
	for _, m := range hidden {
		if m == id {
			return true
		}
	}
	return false
}

func (h *Handler) shape(body map[string]any, resolved string) map[string]any {
	out := make(map[string]any, len(body)+2)
	for k, v := range body {
		out[k] = v
	}
	out["model"] = resolved
	if v, ok := out["max_completion_tokens"]; ok {
		if _, has := out["max_tokens"]; !has {
			out["max_tokens"] = v
		}
		delete(out, "max_completion_tokens")
	}
	h.St.Mu.RLock()
	def := h.St.Data.Config.DefaultMaxTokens
	h.St.Mu.RUnlock()
	// Zero follows the upstream default. Never silently impose a small output
	// cap on clients that intentionally omitted token limits.
	if _, ok := out["max_tokens"]; !ok && def > 0 {
		out["max_tokens"] = def
	}
	if stream, _ := out["stream"].(bool); !stream {
		delete(out, "stream_options")
	}
	out["n"] = 1 // upstream supports one choice; fan-out is done locally
	return out
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	switch {
	case path == "/v1/models" || path == "/models":
		h.listModels(w, r)
	case strings.HasPrefix(path, "/v1/models/") || strings.HasPrefix(path, "/models/"):
		h.getModel(w, r, path[strings.LastIndex(path, "/")+1:])
	case path == "/v1/chat/completions" || path == "/chat/completions":
		h.chat(w, r)
	case path == "/v1/completions" || path == "/completions":
		h.legacy(w, r)
	case path == "/v1/responses" || path == "/responses":
		h.responses(w, r)
	default:
		fail(w, 404, "未知路径 "+r.Method+" "+path, "invalid_request_error", "unknown_url")
	}
}

func method(w http.ResponseWriter, r *http.Request, allowed string) bool {
	if r.Method == allowed {
		return true
	}
	w.Header().Set("Allow", allowed)
	fail(w, 405, "只支持 "+allowed, "invalid_request_error", "method_not_allowed")
	return false
}

func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request) (caller, bool) {
	c, msg := h.resolveCaller(r)
	if msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return c, false
	}
	return c, true
}

func (h *Handler) modelCatalog(ctx context.Context, byok string) catalog.Snapshot {
	if h.Catalog == nil {
		return catalog.Builtin()
	}
	return h.Catalog.Get(ctx, byok)
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodGet) {
		return
	}
	c, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	snapshot := h.modelCatalog(r.Context(), c.Byok)
	h.St.Mu.RLock()
	data := []any{}
	seen := map[string]bool{}
	available := map[string]bool{}
	for _, m := range snapshot.Data {
		available[m.ID] = true
		if !hiddenIn(m.ID, h.St.Data.Config.HiddenModels) {
			data = append(data, m)
			seen[m.ID] = true
		}
	}
	aliases := make([]string, 0, len(h.St.Data.Config.Aliases))
	for a := range h.St.Data.Config.Aliases {
		aliases = append(aliases, a)
	}
	sort.Strings(aliases)
	for _, a := range aliases {
		target := h.St.Data.Config.Aliases[a]
		if !seen[a] && available[target] && !hiddenIn(a, h.St.Data.Config.HiddenModels) && !hiddenIn(target, h.St.Data.Config.HiddenModels) {
			data = append(data, card(a, "meoo-alias"))
		}
	}
	h.St.Mu.RUnlock()
	w.Header().Set("X-Meoo-Models-Source", snapshot.Source)
	w.Header().Set("X-Meoo-Models-Stale", fmt.Sprint(snapshot.Stale))
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request, id string) {
	if !method(w, r, http.MethodGet) {
		return
	}
	c, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	snapshot := h.modelCatalog(r.Context(), c.Byok)
	h.St.Mu.RLock()
	target, alias := h.St.Data.Config.Aliases[strings.ToLower(id)]
	hidden := hiddenIn(id, h.St.Data.Config.HiddenModels)
	if alias {
		hidden = hidden || hiddenIn(target, h.St.Data.Config.HiddenModels)
	}
	h.St.Mu.RUnlock()
	for _, entry := range snapshot.Data {
		if hidden {
			break
		}
		if entry.ID == id {
			writeJSON(w, 200, entry)
			return
		}
		if alias && entry.ID == target {
			writeJSON(w, 200, card(id, "meoo-alias"))
			return
		}
	}
	fail(w, 404, "模型 "+id+" 不存在", "invalid_request_error", "model_not_found")
}

const maxRequestBytes = 8 << 20
const maxResponseBytes = 32 << 20

func readRequest(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	dec := json.NewDecoder(r.Body)
	var body map[string]any
	err := dec.Decode(&body)
	if err == nil && body != nil {
		var extra any
		err = dec.Decode(&extra)
		if err == io.EOF {
			return body, true
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		fail(w, 413, "请求体超过大小上限", "invalid_request_error", "request_too_large")
	} else {
		fail(w, 400, "请求体须为单个 JSON 对象", "invalid_request_error", "invalid_json")
	}
	return nil, false
}

func requestError(msg string) error {
	return &upstream.Error{Status: 400, Type: "invalid_request_error", Code: "invalid_parameter", Message: msg}
}

func validateCommon(body map[string]any) (int, error) {
	if v, exists := body["model"]; exists {
		if _, ok := v.(string); !ok {
			return 0, requestError("'model' must be a string")
		}
	}
	n := 1
	if v, exists := body["n"]; exists {
		f, ok := v.(float64)
		if !ok || f != math.Trunc(f) || f < 1 || f > 4 {
			return 0, requestError("'n' must be an integer between 1 and 4")
		}
		n = int(f)
	}
	if v, exists := body["stream"]; exists {
		if _, ok := v.(bool); !ok {
			return 0, requestError("'stream' must be a boolean")
		}
	}
	if v, exists := body["stream_options"]; exists && v != nil {
		so, ok := v.(map[string]any)
		if !ok {
			return 0, requestError("'stream_options' must be an object")
		}
		if v, exists := so["include_usage"]; exists {
			if _, ok := v.(bool); !ok {
				return 0, requestError("stream_options.include_usage must be a boolean")
			}
		}
	}
	for _, name := range []string{"max_tokens", "max_completion_tokens"} {
		if v, exists := body[name]; exists {
			f, ok := v.(float64)
			if !ok || f <= 0 || f != math.Trunc(f) {
				return 0, requestError("'" + name + "' must be a positive integer")
			}
		}
	}
	return n, nil
}

func respondError(w http.ResponseWriter, err error) {
	e := asError(err)
	fail(w, e.Status, e.Message, e.Type, e.Code)
}

func asError(err error) *upstream.Error {
	var e *upstream.Error
	if errors.As(err, &e) {
		return e
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &upstream.Error{Status: 504, Type: "api_error", Code: "upstream_timeout", Message: "上游超时", Cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &upstream.Error{Status: 499, Type: "api_error", Code: "request_cancelled", Message: "请求已取消", Cause: err}
	}
	return &upstream.Error{Status: 502, Type: "api_error", Code: "invalid_upstream_response", Message: err.Error(), Cause: err}
}

func (h *Handler) chat(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	c, ok := h.authenticate(w, r)
	if !ok {
		return
	}
	body, ok := readRequest(w, r)
	if !ok {
		return
	}
	n, err := validateCommon(body)
	if err != nil {
		respondError(w, err)
		return
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		respondError(w, requestError("'messages' must be a non-empty array"))
		return
	}
	for _, msg := range msgs {
		m, ok := msg.(map[string]any)
		if !ok {
			respondError(w, requestError("each message must be an object"))
			return
		}
		role, _ := m["role"].(string)
		if role == "" {
			respondError(w, requestError("each message requires a role"))
			return
		}
	}
	name, _ := body["model"].(string)
	resolved := h.resolveModel(name)
	payload := h.shape(body, resolved)
	if stream, _ := payload["stream"].(bool); stream {
		h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
			return run.chatStream(w, payload, n, nil)
		})
		return
	}
	h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
		d, err := run.complete(payload, n)
		if err != nil {
			return false, 0, 0, err
		}
		pt, ct := tokens(d)
		writeJSON(w, 200, d)
		return true, pt, ct, nil
	})
}

type callRun struct {
	h      *Handler
	r      *http.Request
	c      caller
	picked *model.Key
	key    string
	label  string
	model  string
	start  time.Time
}

func (h *Handler) startRun(w http.ResponseWriter, r *http.Request, c caller, resolved string, exclude map[string]bool) *callRun {
	run := &callRun{h: h, r: r, c: c, key: c.Byok, label: c.Label, model: resolved, start: time.Now()}
	if run.key == "" {
		identity := ""
		if c.ClientKey != nil {
			identity = c.ClientKey.ID
		}
		run.picked = h.Picker.PickForExcluding(identity, exclude)
		if run.picked == nil {
			err := &upstream.Error{Status: 503, Type: "api_error", Code: "no_available_key", Message: "当前没有可用 key（全部禁用、冷却或余额不足）"}
			run.finish(err, 0, 0)
			respondError(w, err)
			return nil
		}
		run.key, run.label = run.picked.Key, run.picked.Label
	}
	return run
}

func (run *callRun) finish(err error, pt, ct int64) {
	status, msg := 200, ""
	if err != nil {
		e := asError(err)
		status, msg = e.Status, e.Message
		if run.key != "" {
			msg = strings.ReplaceAll(msg, run.key, "[redacted]")
			e.Message = msg
		}
	}
	cancelled := run.r.Context().Err() != nil || status == 499
	if cancelled {
		status = 499
	}
	if !cancelled {
		run.recordKey(err == nil, err, pt, ct)
	}
	if run.c.ClientKey != nil {
		run.h.St.Mu.Lock()
		for _, c := range run.h.St.Data.ClientKeys {
			if c != nil && c.ID == run.c.ClientKey.ID && c.Key == run.c.ClientKey.Key {
				c.Stats.Req++
				if status < 400 {
					c.Stats.OK++
				} else {
					c.Stats.Fail++
				}
				c.Stats.PT += pt
				c.Stats.CT += ct
				c.Stats.LastUsed = time.Now().UnixMilli()
				break
			}
		}
		run.h.St.Mu.Unlock()
	}
	run.h.St.Record(&model.LogRow{Rid: rid(run.r), Method: run.r.Method, Path: run.r.URL.Path, Status: status,
		Ms: time.Since(run.start).Milliseconds(), Model: run.model, KeyLabel: run.label,
		Byok: run.c.Byok != "", PT: pt, CT: ct, Error: msg})
}

func tokenInt(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func tokens(d map[string]any) (pt, ct int64) {
	if u, ok := d["usage"].(map[string]any); ok {
		pt = tokenInt(u["prompt_tokens"])
		ct = tokenInt(u["completion_tokens"])
	}
	return
}

func (h *Handler) timeout() time.Duration {
	h.St.Mu.RLock()
	ms := h.St.Data.Config.TimeoutMs
	h.St.Mu.RUnlock()
	if ms <= 0 {
		ms = 180000
	}
	return time.Duration(ms) * time.Millisecond
}

func decodeCompletion(r io.Reader) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("upstream response exceeds size limit")
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil || d == nil {
		return nil, fmt.Errorf("上游返回非 JSON 对象")
	}
	if e, ok := d["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return nil, &upstream.Error{Status: 502, Type: "api_error", Code: "upstream_error", Message: msg}
	}
	choices, ok := d["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, fmt.Errorf("上游缺少 choices")
	}
	for _, choice := range choices {
		ch, ok := choice.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("上游 choices 格式错误")
		}
		if _, ok := ch["message"].(map[string]any); !ok {
			return nil, fmt.Errorf("上游缺少 message")
		}
	}
	return d, nil
}

func (run *callRun) complete(payload map[string]any, n int) (map[string]any, error) {
	ctx, cancel := context.WithCancel(run.r.Context())
	defer cancel()
	type result struct {
		i   int
		d   map[string]any
		err error
	}
	ch := make(chan result, n)
	for i := 0; i < n; i++ {
		go func(index int) {
			resp, err := run.h.Up.Chat(ctx, payload, run.key, run.h.timeout(), false)
			var d map[string]any
			if err == nil {
				d, err = decodeCompletion(resp.Body)
				resp.Body.Close()
			}
			ch <- result{index, d, err}
		}(i)
	}
	results := make([]map[string]any, n)
	var firstErr error
	for i := 0; i < n; i++ {
		r := <-ch
		if r.err != nil && firstErr == nil {
			firstErr = r.err
			cancel()
		}
		results[r.i] = r.d
	}
	if firstErr != nil {
		return nil, firstErr
	}
	merged := results[0]
	if _, ok := merged["model"]; !ok {
		merged["model"] = run.model
	}
	if n == 1 {
		return merged, nil
	}
	choices := []any{}
	var pt, ct int64
	for i, d := range results {
		cs := d["choices"].([]any)
		if len(cs) != 1 {
			return nil, fmt.Errorf("上游单次请求应返回一个 choice")
		}
		m := cs[0].(map[string]any)
		m["index"] = i
		choices = append(choices, m)
		p, c := tokens(d)
		pt += p
		ct += c
	}
	merged["choices"] = choices
	merged["usage"] = map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
	return merged, nil
}

func rid(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		if len(v) > 128 {
			v = v[:128]
		}
		return v
	}
	return model.RandID("", 8)
}
