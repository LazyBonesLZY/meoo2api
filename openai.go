package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var NativeModels = []string{
	"qwen3.8-flash", "qwen3.8-max", "qwen3.7-max", "qwen3.7-plus",
	"deepseek-v4.1-flash-1017", "deepseek-v4-pro-0813", "deepseek-v4-flash-0731",
	"glm-5.2-fast-preview", "glm-5.2",
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oaErr(w http.ResponseWriter, status int, msg, typ, code string) {
	e := map[string]any{"message": msg, "type": typ}
	if code != "" {
		e["code"] = code
	}
	writeJSON(w, status, map[string]any{"error": e})
}

func (s *State) ResolveModel(m string) string {
	if m == "" {
		return "qwen3.8-flash"
	}
	if v, ok := s.Config.Aliases[strings.ToLower(strings.TrimSpace(m))]; ok {
		return v
	}
	return strings.TrimSpace(m)
}

func (s *State) KnownModel(id string) bool {
	for _, m := range NativeModels {
		if m == id {
			return true
		}
	}
	_, ok := s.Config.Aliases[strings.ToLower(id)]
	return ok
}

func (s *State) Hidden(id string) bool {
	for _, m := range s.Config.HiddenModels {
		if m == id {
			return true
		}
	}
	return false
}

func modelCard(id, owner string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 0, "owned_by": owner}
}

type caller struct {
	Byok      string
	Label     string
	ClientKey *ClientKey
}

func (s *State) ResolveCaller(r *http.Request) (caller, string) {
	tok := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if tok == "" {
		return caller{}, "缺少 Authorization 头"
	}
	if IsProjectAK(tok) {
		return caller{Byok: tok, Label: "BYOK"}, ""
	}
	if strings.HasPrefix(tok, "meoo_ak_") {
		return caller{}, "个人 AK 不能调用模型网关，请用项目服务 AK（meoo_project_…）"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, c := range s.ClientKeys {
		if c.Enabled && c.Key == tok {
			return caller{ClientKey: c, Label: c.Label}, ""
		}
	}
	if len(s.Keys) == 0 && len(s.ClientKeys) == 0 {
		return caller{}, "服务未配置 key（控制台添加或使用 BYOK）"
	}
	return caller{}, "无效的 API key"
}

func (p *Picker) Take(c caller) (*Key, string) {
	if c.Byok != "" {
		return nil, "BYOK"
	}
	k := p.Pick()
	if k == nil {
		return nil, ""
	}
	return k, k.Label
}

func (s *State) ShapeChat(in map[string]any, model string) map[string]any {
	out := map[string]any{}
	for k, v := range in {
		out[k] = v
	}
	out["model"] = model
	if _, ok := out["messages"]; !ok {
		out["messages"] = []any{}
	}
	if v, ok := out["max_completion_tokens"]; ok {
		if _, has := out["max_tokens"]; !has {
			out["max_tokens"] = v
		}
		delete(out, "max_completion_tokens")
	}
	if _, ok := out["max_tokens"]; !ok {
		out["max_tokens"] = s.Config.DefaultMaxTokens
	}
	stream, _ := out["stream"].(bool)
	if !stream {
		delete(out, "stream_options")
	} else if so, ok := out["stream_options"].(map[string]any); ok {
		so["include_usage"] = true
		out["stream_options"] = so
	}
	out["n"] = 1
	return out
}

func (s *State) handleModels(w http.ResponseWriter, r *http.Request, path string) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if path == "/v1/models" || path == "/models" {
		data := []any{}
		for _, m := range NativeModels {
			if !s.Hidden(m) {
				data = append(data, modelCard(m, "meoo"))
			}
		}
		for a := range s.Config.Aliases {
			if !s.Hidden(a) {
				data = append(data, modelCard(a, "meoo-alias"))
			}
		}
		writeJSON(w, 200, map[string]any{"object": "list", "data": data})
		return true
	}
	if strings.HasPrefix(path, "/v1/models/") {
		id := strings.TrimPrefix(path, "/v1/models/")
		if !s.KnownModel(id) || s.Hidden(id) {
			oaErr(w, 404, "模型 "+id+" 不存在", "invalid_request_error", "model_not_found")
			return true
		}
		owner := "meoo"
		if _, ok := s.Config.Aliases[strings.ToLower(id)]; ok {
			owner = "meoo-alias"
			id = s.Config.Aliases[strings.ToLower(id)]
		}
		writeJSON(w, 200, modelCard(id, owner))
		return true
	}
	return false
}

func (s *State) doChat(w http.ResponseWriter, r *http.Request, c caller, key *Key, model string, payload map[string]any) {
	start := time.Now()
	keyStr, keyLabel := "", c.Label
	if c.Byok != "" {
		keyStr = c.Byok
	} else if key != nil {
		keyStr = key.Key
	}
	if keyStr == "" {
		s.Record(&LogRow{Method: r.Method, Path: r.URL.Path, Status: 503, KeyLabel: keyLabel})
		oaErr(w, 503, "当前没有可用 key（全部禁用或在冷却中）", "api_error", "no_available_key")
		return
	}
	stream, _ := payload["stream"].(bool)
	n, _ := payload["n"].(float64)

	if n > 1 && stream {
		oaErr(w, 400, "n>1 不支持 stream=true", "invalid_request_error", "invalid_parameter_error")
		return
	}
	if n > 1 {
		single := map[string]any{}
		for k, v := range payload {
			single[k] = v
		}
		single["n"] = 1
		type res struct {
			data map[string]any
			err  error
		}
		ch := make(chan res, int(n))
		for i := 0; i < int(n); i++ {
			go func() {
				resp, err := Call(r.Context(), "/chat/completions", single, keyStr, time.Duration(s.Config.TimeoutMs)*time.Millisecond, false)
				if err != nil {
					ch <- res{err: err}
					return
				}
				defer resp.Body.Close()
				var d map[string]any
				_ = json.NewDecoder(resp.Body).Decode(&d)
				ch <- res{data: d}
			}()
		}
		merged := map[string]any{}
		choices := []any{}
		for i := 0; i < int(n); i++ {
			rr := <-ch
			if rr.err != nil {
				s.MarkKey(key, false, true)
				s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 502, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: keyLabel, Byok: c.Byok != "", Error: rr.err.Error()})
				oaErr(w, 502, rr.err.Error(), "api_error", "")
				return
			}
			for k, v := range rr.data {
				merged[k] = v
			}
			if cs, ok := rr.data["choices"].([]any); ok {
				for j, ch1 := range cs {
					if m, ok := ch1.(map[string]any); ok {
						m["index"] = i
						_ = j
						choices = append(choices, m)
					}
				}
			}
		}
		merged["choices"] = choices
		merged["model"] = model
		s.MarkKey(key, true, false)
		s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 200, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: keyLabel, Byok: c.Byok != ""})
		writeJSON(w, 200, merged)
		return
	}

	resp, err := Call(r.Context(), "/chat/completions", payload, keyStr, time.Duration(s.Config.TimeoutMs)*time.Millisecond, stream)
	if err != nil {
		up, _ := err.(*UpErr)
		status, typ, msg := 502, "api_error", err.Error()
		if up != nil {
			status, typ, msg = up.Status, up.Type, up.Message
		}
		s.MarkKey(key, false, status == 429 || status >= 500)
		s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: status, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: keyLabel, Byok: c.Byok != "", Error: msg})
		oaErr(w, status, msg, typ, "")
		return
	}
	defer resp.Body.Close()
	s.MarkKey(key, true, false)

	if !stream {
		raw, _ := io.ReadAll(resp.Body)
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 502, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: keyLabel})
			oaErr(w, 502, "上游返回非 JSON", "api_error", "")
			return
		}
		if _, ok := d["model"]; !ok {
			d["model"] = model
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
		s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 200, Ms: time.Since(start).Milliseconds(), Model: model, PT: pt, CT: ct, KeyLabel: keyLabel, Byok: c.Byok != ""})
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
		nn, rerr := resp.Body.Read(buf)
		if nn > 0 {
			if _, werr := w.Write(buf[:nn]); werr != nil {
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
	s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 200, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: keyLabel, Byok: c.Byok != ""})
}

func traceID(r *http.Request) string {
	if v := r.Header.Get("X-Request-Id"); v != "" {
		return v
	}
	return randID("", 8)
}

func (s *State) handleLegacyCompletion(w http.ResponseWriter, r *http.Request, c caller, key *Key, in map[string]any) {
	keyStr := c.Byok
	if key != nil {
		keyStr = key.Key
	}
	prompt := ""
	if v, ok := in["prompt"].(string); ok {
		prompt = v
	} else if arr, ok := in["prompt"].([]any); ok {
		parts := []string{}
		for _, a := range arr {
			if sv, ok := a.(string); ok {
				parts = append(parts, sv)
			}
		}
		prompt = strings.Join(parts, "\n")
	}
	if prompt == "" {
		oaErr(w, 400, "'prompt' is a required property", "invalid_request_error", "missing_prompt")
		return
	}
	model := s.ResolveModel(fmt.Sprint(in["model"]))
	payload := map[string]any{
		"model":    model,
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	}
	for _, f := range []string{"max_tokens", "temperature", "top_p", "stop"} {
		if v, ok := in[f]; ok {
			payload[f] = v
		}
	}
	payload = s.ShapeChat(payload, model)
	start := time.Now()
	resp, err := Call(r.Context(), "/chat/completions", payload, keyStr, time.Duration(s.Config.TimeoutMs)*time.Millisecond, false)
	if err != nil {
		s.MarkKey(key, false, true)
		oaErr(w, 502, err.Error(), "api_error", "")
		return
	}
	defer resp.Body.Close()
	var d map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&d)
	s.MarkKey(key, true, false)
	s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 200, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: c.Label})
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
	id := randID("cmpl-", 8)
	if v, ok := d["id"].(string); ok {
		id = v
	}
	writeJSON(w, 200, map[string]any{
		"id": id, "object": "text_completion", "created": time.Now().Unix(),
		"model": model, "choices": choices, "usage": d["usage"],
	})
}

func (s *State) handleResponses(w http.ResponseWriter, r *http.Request, c caller, key *Key, in map[string]any) {
	keyStr := c.Byok
	if key != nil {
		keyStr = key.Key
	}
	msgs := []any{}
	switch v := in["input"].(type) {
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
	if inst, ok := in["instructions"].(string); ok && inst != "" {
		msgs = append([]any{map[string]any{"role": "system", "content": inst}}, msgs...)
	}
	model := s.ResolveModel(fmt.Sprint(in["model"]))
	payload := map[string]any{"model": model, "messages": msgs}
	if v, ok := in["max_output_tokens"]; ok {
		payload["max_tokens"] = v
	}
	payload = s.ShapeChat(payload, model)
	start := time.Now()
	resp, err := Call(r.Context(), "/chat/completions", payload, keyStr, time.Duration(s.Config.TimeoutMs)*time.Millisecond, false)
	if err != nil {
		s.MarkKey(key, false, true)
		oaErr(w, 502, err.Error(), "api_error", "")
		return
	}
	defer resp.Body.Close()
	var d map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&d)
	s.MarkKey(key, true, false)
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
	s.Record(&LogRow{Rid: traceID(r), Method: r.Method, Path: r.URL.Path, Status: 200, Ms: time.Since(start).Milliseconds(), Model: model, KeyLabel: c.Label})
	usage := map[string]any{}
	if u, ok := d["usage"].(map[string]any); ok {
		usage["input_tokens"] = u["prompt_tokens"]
		usage["output_tokens"] = u["completion_tokens"]
		usage["total_tokens"] = u["total_tokens"]
	}
	writeJSON(w, 200, map[string]any{
		"id": "resp_" + randID("", 8), "object": "response", "created_at": time.Now().Unix(),
		"status": "completed", "model": model,
		"output": []any{map[string]any{
			"id": "msg_" + randID("", 8), "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
		}},
		"output_text": text, "usage": usage,
	})
}

func parseIntDefault(s string, d int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return d
	}
	return v
}
