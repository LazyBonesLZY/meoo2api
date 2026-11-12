package gateway

import (
	"net/http"
	"time"

	"meoo2api/internal/model"
)

func copyFields(dst, src map[string]any, fields ...string) {
	for _, f := range fields {
		if v, ok := src[f]; ok {
			dst[f] = v
		}
	}
}

func (h *Handler) legacy(w http.ResponseWriter, r *http.Request) {
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
	prompts := []string{}
	switch p := body["prompt"].(type) {
	case string:
		prompts = append(prompts, p)
	case []any:
		for _, v := range p {
			s, ok := v.(string)
			if !ok {
				respondError(w, requestError("token-array prompts are not supported"))
				return
			}
			prompts = append(prompts, s)
		}
	}
	if len(prompts) == 0 || len(prompts) > 16 {
		respondError(w, requestError("'prompt' must be a string or an array of 1 to 16 strings"))
		return
	}
	allowed := map[string]bool{}
	for _, f := range []string{"prompt", "model", "n", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "presence_penalty", "frequency_penalty", "seed", "user", "stream", "stream_options", "logit_bias", "suffix", "logprobs", "echo", "best_of"} {
		allowed[f] = true
	}
	for f := range body {
		if !allowed[f] {
			respondError(w, requestError("unsupported legacy parameter: "+f))
			return
		}
	}
	for _, field := range []string{"suffix", "logprobs"} {
		if v := body[field]; v != nil {
			respondError(w, requestError("legacy '"+field+"' is not supported"))
			return
		}
	}
	if v, exists := body["echo"]; exists && v != false {
		respondError(w, requestError("legacy 'echo' supports only false"))
		return
	}
	if v, exists := body["best_of"]; exists && v != float64(n) {
		respondError(w, requestError("'best_of' must equal n"))
		return
	}
	stream, _ := body["stream"].(bool)
	if stream && len(prompts) != 1 {
		respondError(w, requestError("streaming prompt batches are not supported"))
		return
	}
	name, _ := body["model"].(string)
	resolved := h.resolveModel(name)
	payload := map[string]any{"model": resolved}
	copyFields(payload, body, "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "presence_penalty", "frequency_penalty", "seed", "user", "stream", "stream_options", "logit_bias")
	payload = h.shape(payload, resolved)
	payload["messages"] = []any{map[string]any{"role": "user", "content": prompts[0]}}
	if stream {
		h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
			return run.chatStream(w, payload, n, legacyChunk)
		})
		return
	}
	h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
		choices := []any{}
		var pt, ct int64
		var first map[string]any
		for _, prompt := range prompts {
			payload["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
			d, err := run.complete(payload, n)
			if err != nil {
				return false, pt, ct, err
			}
			if first == nil {
				first = d
			}
			p, c := tokens(d)
			pt += p
			ct += c
			for _, ch := range d["choices"].([]any) {
				m := ch.(map[string]any)
				message := m["message"].(map[string]any)
				text, _ := message["content"].(string)
				choices = append(choices, map[string]any{"text": text, "index": len(choices), "logprobs": nil, "finish_reason": m["finish_reason"]})
			}
		}
		out := map[string]any{"id": first["id"], "object": "text_completion", "created": first["created"], "model": resolved, "choices": choices,
			"usage": map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}}
		writeJSON(w, 200, out)
		return true, pt, ct, nil
	})
}

func legacyChunk(d map[string]any) map[string]any {
	out := map[string]any{"object": "text_completion"}
	copyFields(out, d, "id", "model", "created", "usage", "system_fingerprint")
	choices := []any{}
	if cs, ok := d["choices"].([]any); ok {
		for _, choice := range cs {
			m, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := m["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			choices = append(choices, map[string]any{"text": text, "index": m["index"], "finish_reason": m["finish_reason"], "logprobs": nil})
		}
	}
	out["choices"] = choices
	return out
}

func responseMessages(input any) ([]any, error) {
	msgs := []any{}
	switch v := input.(type) {
	case string:
		return []any{map[string]any{"role": "user", "content": v}}, nil
	case []any:
		for _, item := range v {
			if text, ok := item.(string); ok {
				msgs = append(msgs, map[string]any{"role": "user", "content": text})
				continue
			}
			m, ok := item.(map[string]any)
			if !ok {
				return nil, requestError("each input item must be a message or function call")
			}
			typ, _ := m["type"].(string)
			switch typ {
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args, ok := m["arguments"].(string)
				if id == "" || name == "" || !ok {
					return nil, requestError("function_call requires call_id, name and arguments")
				}
				msgs = append(msgs, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": args}}}})
			case "function_call_output":
				id, _ := m["call_id"].(string)
				output, ok := m["output"].(string)
				if id == "" || !ok {
					return nil, requestError("function_call_output requires call_id and string output")
				}
				msgs = append(msgs, map[string]any{"role": "tool", "tool_call_id": id, "content": output})
			case "", "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				if role != "user" && role != "assistant" && role != "system" && role != "developer" {
					return nil, requestError("invalid input message role")
				}
				var content any
				switch cv := m["content"].(type) {
				case string:
					content = cv
				case []any:
					parts := []any{}
					for _, part := range cv {
						pm, ok := part.(map[string]any)
						if !ok {
							return nil, requestError("invalid input content part")
						}
						switch pm["type"] {
						case "input_text", "output_text", "text":
							text, ok := pm["text"].(string)
							if !ok {
								return nil, requestError("text content requires text")
							}
							parts = append(parts, map[string]any{"type": "text", "text": text})
						case "input_image":
							url, _ := pm["image_url"].(string)
							if url == "" {
								return nil, requestError("input_image requires image_url")
							}
							image := map[string]any{"url": url}
							copyFields(image, pm, "detail")
							parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
						default:
							return nil, requestError("unsupported input content type")
						}
					}
					content = parts
				default:
					return nil, requestError("input message requires content")
				}
				msgs = append(msgs, map[string]any{"role": role, "content": content})
			default:
				return nil, requestError("unsupported input item type")
			}
		}
	default:
		return nil, requestError("'input' must be a string or array")
	}
	if len(msgs) == 0 {
		return nil, requestError("'input' must contain a message")
	}
	return msgs, nil
}

func responsesPayload(body map[string]any) (map[string]any, error) {
	allowed := map[string]bool{}
	for _, f := range []string{"input", "model", "instructions", "tools", "tool_choice", "max_output_tokens", "temperature", "top_p", "parallel_tool_calls", "stream", "user", "text", "store", "background", "include", "previous_response_id", "conversation"} {
		allowed[f] = true
	}
	for f := range body {
		if !allowed[f] {
			return nil, requestError("unsupported Responses parameter: " + f)
		}
	}
	for _, f := range []string{"previous_response_id", "conversation", "include"} {
		if v := body[f]; v != nil {
			return nil, requestError("'" + f + "' is not supported by this stateless gateway")
		}
	}
	for _, f := range []string{"background", "store"} {
		if v, ok := body[f]; ok && v != false {
			return nil, requestError("'" + f + "' supports only false")
		}
	}
	msgs, err := responseMessages(body["input"])
	if err != nil {
		return nil, err
	}
	if inst, exists := body["instructions"]; exists && inst != nil {
		s, ok := inst.(string)
		if !ok {
			return nil, requestError("instructions must be a string")
		}
		if s != "" {
			msgs = append([]any{map[string]any{"role": "system", "content": s}}, msgs...)
		}
	}
	p := map[string]any{"messages": msgs}
	copyFields(p, body, "model", "temperature", "top_p", "parallel_tool_calls", "stream", "user")
	if v, ok := body["max_output_tokens"]; ok {
		p["max_tokens"] = v
	}
	if tools, exists := body["tools"]; exists {
		list, ok := tools.([]any)
		if !ok {
			return nil, requestError("tools must be an array")
		}
		converted := []any{}
		for _, tool := range list {
			m, ok := tool.(map[string]any)
			if !ok || m["type"] != "function" {
				return nil, requestError("only function tools are supported")
			}
			name, _ := m["name"].(string)
			if name == "" {
				return nil, requestError("function tool requires name")
			}
			fn := map[string]any{"name": name}
			copyFields(fn, m, "description", "parameters", "strict")
			converted = append(converted, map[string]any{"type": "function", "function": fn})
		}
		p["tools"] = converted
	}
	if tc, exists := body["tool_choice"]; exists {
		if m, ok := tc.(map[string]any); ok {
			name, _ := m["name"].(string)
			if m["type"] != "function" || name == "" {
				return nil, requestError("unsupported tool_choice")
			}
			p["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		} else if tc == "auto" || tc == "none" || tc == "required" {
			p["tool_choice"] = tc
		} else {
			return nil, requestError("invalid tool_choice")
		}
	}
	if v, exists := body["text"]; exists {
		text, ok := v.(map[string]any)
		if !ok {
			return nil, requestError("text must be an object")
		}
		for k := range text {
			if k != "format" {
				return nil, requestError("unsupported text parameter: " + k)
			}
		}
		if v, exists := text["format"]; exists {
			format, ok := v.(map[string]any)
			if !ok {
				return nil, requestError("text.format must be an object")
			}
			typ, _ := format["type"].(string)
			switch typ {
			case "text", "json_object":
				p["response_format"] = map[string]any{"type": typ}
			case "json_schema":
				schema := map[string]any{}
				copyFields(schema, format, "name", "description", "schema", "strict")
				p["response_format"] = map[string]any{"type": typ, "json_schema": schema}
			default:
				return nil, requestError("unsupported text format")
			}
		}
	}
	return p, nil
}

func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
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
	payload, err := responsesPayload(body)
	if err != nil {
		respondError(w, err)
		return
	}
	if _, err = validateCommon(payload); err != nil {
		respondError(w, err)
		return
	}
	name, _ := payload["model"].(string)
	resolved := h.resolveModel(name)
	payload = h.shape(payload, resolved)
	if stream, _ := payload["stream"].(bool); stream {
		payload["stream_options"] = map[string]any{"include_usage": true}
		h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
			adapter := newResponsesStream(w, resolved)
			return run.chatStream(adapter, payload, 1, nil)
		})
		return
	}
	h.withFailover(w, r, c, resolved, func(run *callRun) (bool, int64, int64, error) {
		d, err := run.complete(payload, 1)
		if err != nil {
			return false, 0, 0, err
		}
		pt, ct := tokens(d)
		writeJSON(w, 200, responseObject(d, resolved, "resp_"+model.RandID("", 8)))
		return true, pt, ct, nil
	})
}

func responseObject(d map[string]any, resolved, id string) map[string]any {
	output := []any{}
	text := ""
	status := "completed"
	var incomplete any
	if cs, ok := d["choices"].([]any); ok && len(cs) > 0 {
		ch, _ := cs[0].(map[string]any)
		msg, _ := ch["message"].(map[string]any)
		text, _ = msg["content"].(string)
		if ch["finish_reason"] == "length" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		}
		if text != "" {
			output = append(output, map[string]any{"id": "msg_" + model.RandID("", 8), "type": "message", "role": "assistant", "status": status,
				"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}})
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, call := range calls {
				m, _ := call.(map[string]any)
				fn, _ := m["function"].(map[string]any)
				output = append(output, map[string]any{"id": "fc_" + model.RandID("", 8), "type": "function_call", "status": "completed",
					"call_id": m["id"], "name": fn["name"], "arguments": fn["arguments"]})
			}
		}
	}
	pt, ct := tokens(d)
	return map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": resolved,
		"error": nil, "incomplete_details": incomplete, "output": output, "output_text": text,
		"usage": map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct}}
}
