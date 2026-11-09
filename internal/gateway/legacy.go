package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
)

func (h *Handler) legacyKey(r *http.Request, c caller) (string, *model.Key) {
	if c.Byok != "" {
		return c.Byok, nil
	}
	k := h.Picker.Pick()
	if k == nil {
		return "", nil
	}
	return k.Key, k
}

func (h *Handler) legacy(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	c, msg := h.resolveCaller(r)
	if msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	prompt := ""
	switch v := body["prompt"].(type) {
	case string:
		prompt = v
	case []any:
		parts := []string{}
		for _, a := range v {
			if sv, ok := a.(string); ok {
				parts = append(parts, sv)
			}
		}
		prompt = strings.Join(parts, "\n")
	}
	if prompt == "" {
		fail(w, 400, "'prompt' is a required property", "invalid_request_error", "missing_prompt")
		return
	}
	keyStr, picked := h.legacyKey(r, c)
	if keyStr == "" {
		fail(w, 503, "当前没有可用 key", "api_error", "no_available_key")
		return
	}
	resolved := h.resolveModel(fmt.Sprint(body["model"]))
	payload := map[string]any{
		"model":    resolved,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}
	for _, f := range []string{"max_tokens", "temperature", "top_p", "stop"} {
		if v, ok := body[f]; ok {
			payload[f] = v
		}
	}
	payload = h.shape(payload, resolved)
	start := time.Now()
	resp, err := h.Up.Chat(r.Context(), payload, keyStr, h.timeout(), false)
	if err != nil {
		h.Picker.MarkKey(picked, false, true)
		fail(w, 502, err.Error(), "api_error", "")
		return
	}
	defer resp.Body.Close()
	var d map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&d)
	h.Picker.MarkKey(picked, true, false)
	h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: 200,
		Ms: time.Since(start).Milliseconds(), Model: resolved, KeyLabel: c.Label})
	choices := []any{}
	if cs, ok := d["choices"].([]any); ok {
		for i, ch := range cs {
			m, _ := ch.(map[string]any)
			text := ""
			if msg, ok := m["message"].(map[string]any); ok {
				if cv, ok := msg["content"].(string); ok {
					text = cv
				}
			}
			choices = append(choices, map[string]any{"text": text, "index": i, "logprobs": nil, "finish_reason": m["finish_reason"]})
		}
	}
	id := model.RandID("cmpl-", 8)
	if v, ok := d["id"].(string); ok {
		id = v
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "object": "text_completion", "created": time.Now().Unix(),
		"model": resolved, "choices": choices, "usage": d["usage"],
	})
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	c, msg := h.resolveCaller(r)
	if msg != "" {
		fail(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	msgs := []any{}
	switch v := body["input"].(type) {
	case string:
		msgs = append(msgs, map[string]any{"role": "user", "content": v})
	case []any:
		for _, item := range v {
			if sv, ok := item.(string); ok {
				msgs = append(msgs, map[string]any{"role": "user", "content": sv})
				continue
			}
			if m, ok := item.(map[string]any); ok {
				role := "user"
				if rv, ok := m["role"].(string); ok {
					role = rv
				}
				content := ""
				switch cv := m["content"].(type) {
				case string:
					content = cv
				case []any:
					for _, part := range cv {
						if pm, ok := part.(map[string]any); ok {
							if tv, ok := pm["text"].(string); ok {
								content += tv
							}
						}
					}
				}
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			}
		}
	}
	if inst, ok := body["instructions"].(string); ok && inst != "" {
		msgs = append([]any{map[string]any{"role": "system", "content": inst}}, msgs...)
	}
	keyStr, picked := h.legacyKey(r, c)
	if keyStr == "" {
		fail(w, 503, "当前没有可用 key", "api_error", "no_available_key")
		return
	}
	resolved := h.resolveModel(fmt.Sprint(body["model"]))
	payload := map[string]any{"model": resolved, "messages": msgs}
	if v, ok := body["max_output_tokens"]; ok {
		payload["max_tokens"] = v
	}
	payload = h.shape(payload, resolved)
	start := time.Now()
	resp, err := h.Up.Chat(r.Context(), payload, keyStr, h.timeout(), false)
	if err != nil {
		h.Picker.MarkKey(picked, false, true)
		fail(w, 502, err.Error(), "api_error", "")
		return
	}
	defer resp.Body.Close()
	var d map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&d)
	h.Picker.MarkKey(picked, true, false)
	text := ""
	if cs, ok := d["choices"].([]any); ok && len(cs) > 0 {
		if m, ok := cs[0].(map[string]any); ok {
			if msg, ok := m["message"].(map[string]any); ok {
				if cv, ok := msg["content"].(string); ok {
					text = cv
				}
			}
		}
	}
	h.St.Record(&model.LogRow{Rid: rid(r), Method: r.Method, Path: r.URL.Path, Status: 200,
		Ms: time.Since(start).Milliseconds(), Model: resolved, KeyLabel: c.Label})
	usage := map[string]any{}
	if u, ok := d["usage"].(map[string]any); ok {
		usage["input_tokens"] = u["prompt_tokens"]
		usage["output_tokens"] = u["completion_tokens"]
		usage["total_tokens"] = u["total_tokens"]
	}
	writeJSON(w, 200, map[string]any{
		"id": "resp_" + model.RandID("", 8), "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": resolved,
		"output": []any{map[string]any{
			"id": "msg_" + model.RandID("", 8), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"output_text": text, "usage": usage,
	})
}
