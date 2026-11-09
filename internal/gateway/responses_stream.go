package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"meoo2api/internal/model"
)

// responsesStream consumes the validated Chat SSE frames, translating them into
// Responses events. It forwards pre-stream HTTP/JSON errors unchanged.
type responsesStream struct {
	w                       http.ResponseWriter
	stream, started, failed bool
	seq                     int
	id, model, messageID    string
	created                 int64
	text                    strings.Builder
	messageIndex            int
	items                   []map[string]any
	calls                   map[int]*responseCall
	usage                   map[string]any
	finish                  any
}

type responseCall struct {
	item  map[string]any
	index int
}

func newResponsesStream(w http.ResponseWriter, resolved string) *responsesStream {
	return &responsesStream{w: w, id: "resp_" + model.RandID("", 8), model: resolved, created: time.Now().Unix(),
		messageIndex: -1, calls: map[int]*responseCall{}}
}

func (s *responsesStream) Header() http.Header { return s.w.Header() }
func (s *responsesStream) WriteHeader(status int) {
	s.stream = status == 200 && strings.HasPrefix(s.Header().Get("Content-Type"), "text/event-stream")
	s.w.WriteHeader(status)
}
func (s *responsesStream) Flush() {
	if f, ok := s.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *responsesStream) emit(typ string, fields map[string]any) error {
	fields["type"] = typ
	fields["sequence_number"] = s.seq
	s.seq++
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", typ, raw)
	return err
}

func (s *responsesStream) object(status string) map[string]any {
	items := make([]any, len(s.items))
	for i, item := range s.items {
		items[i] = item
	}
	var incomplete any
	if status == "incomplete" {
		incomplete = map[string]any{"reason": "max_output_tokens"}
	}
	return map[string]any{"id": s.id, "object": "response", "created_at": s.created, "status": status,
		"model": s.model, "output": items, "output_text": s.text.String(), "error": nil,
		"incomplete_details": incomplete, "usage": s.usage}
}

func (s *responsesStream) begin() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := s.emit("response.created", map[string]any{"response": s.object("in_progress")}); err != nil {
		return err
	}
	return s.emit("response.in_progress", map[string]any{"response": s.object("in_progress")})
}

func (s *responsesStream) addText(delta string) error {
	if s.messageIndex < 0 {
		s.messageID = "msg_" + model.RandID("", 8)
		s.messageIndex = len(s.items)
		item := map[string]any{"id": s.messageID, "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
		s.items = append(s.items, item)
		if err := s.emit("response.output_item.added", map[string]any{"output_index": s.messageIndex, "item": item}); err != nil {
			return err
		}
		if err := s.emit("response.content_part.added", map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	s.text.WriteString(delta)
	return s.emit("response.output_text.delta", map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0, "delta": delta})
}

func (s *responsesStream) addCall(m map[string]any) error {
	idx, _ := m["index"].(float64)
	call := s.calls[int(idx)]
	fn, _ := m["function"].(map[string]any)
	if call == nil {
		call = &responseCall{index: len(s.items), item: map[string]any{"id": "fc_" + model.RandID("", 8), "type": "function_call", "status": "in_progress", "call_id": m["id"], "name": "", "arguments": ""}}
		s.calls[int(idx)] = call
		s.items = append(s.items, call.item)
		if name, ok := fn["name"].(string); ok {
			call.item["name"] = name
		}
		if err := s.emit("response.output_item.added", map[string]any{"output_index": call.index, "item": call.item}); err != nil {
			return err
		}
	} else if name, ok := fn["name"].(string); ok {
		call.item["name"] = fmt.Sprint(call.item["name"]) + name
	}
	if id, ok := m["id"].(string); ok {
		call.item["call_id"] = id
	}
	if args, ok := fn["arguments"].(string); ok && args != "" {
		call.item["arguments"] = fmt.Sprint(call.item["arguments"]) + args
		return s.emit("response.function_call_arguments.delta", map[string]any{"item_id": call.item["id"], "output_index": call.index, "delta": args})
	}
	return nil
}

func (s *responsesStream) done() error {
	if err := s.begin(); err != nil {
		return err
	}
	status := "completed"
	if s.finish == "length" {
		status = "incomplete"
	}
	if s.messageIndex >= 0 {
		part := map[string]any{"type": "output_text", "text": s.text.String(), "annotations": []any{}}
		fields := map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0, "text": s.text.String()}
		if err := s.emit("response.output_text.done", fields); err != nil {
			return err
		}
		if err := s.emit("response.content_part.done", map[string]any{"item_id": s.messageID, "output_index": s.messageIndex, "content_index": 0, "part": part}); err != nil {
			return err
		}
		s.items[s.messageIndex]["content"] = []any{part}
		s.items[s.messageIndex]["status"] = status
	}
	for _, item := range s.items {
		if item["type"] == "function_call" {
			item["status"] = "completed"
		}
	}
	for i, item := range s.items {
		if item["type"] == "function_call" {
			if err := s.emit("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": i, "arguments": item["arguments"], "name": item["name"]}); err != nil {
				return err
			}
		}
		if err := s.emit("response.output_item.done", map[string]any{"output_index": i, "item": item}); err != nil {
			return err
		}
	}
	return s.emit("response."+status, map[string]any{"response": s.object(status)})
}

func (s *responsesStream) Write(p []byte) (int, error) {
	if !s.stream {
		return s.w.Write(p)
	}
	data := strings.TrimSpace(strings.TrimPrefix(string(p), "data:"))
	if data == "[DONE]" {
		if s.failed {
			return len(p), nil
		}
		return len(p), s.done()
	}
	if !strings.HasPrefix(string(p), "data:") {
		return len(p), nil // upstream heartbeat/comment
	}
	var d map[string]any
	if err := json.Unmarshal([]byte(data), &d); err != nil {
		return 0, err
	}
	if err := s.begin(); err != nil {
		return 0, err
	}
	if e, ok := d["error"]; ok {
		s.failed = true
		if err := s.emit("error", map[string]any{"error": e}); err != nil {
			return 0, err
		}
		obj := s.object("failed")
		obj["error"] = e
		return len(p), s.emit("response.failed", map[string]any{"response": obj})
	}
	if _, ok := d["usage"]; ok {
		pt, ct := tokens(d)
		s.usage = map[string]any{"input_tokens": pt, "output_tokens": ct, "total_tokens": pt + ct}
	}
	if cs, ok := d["choices"].([]any); ok {
		for _, choice := range cs {
			ch, ok := choice.(map[string]any)
			if !ok {
				continue
			}
			if reason := ch["finish_reason"]; reason != nil {
				s.finish = reason
			}
			delta, _ := ch["delta"].(map[string]any)
			if text, ok := delta["content"].(string); ok && text != "" {
				if err := s.addText(text); err != nil {
					return 0, err
				}
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, call := range calls {
					if m, ok := call.(map[string]any); ok {
						if err := s.addCall(m); err != nil {
							return 0, err
						}
					}
				}
			}
		}
	}
	return len(p), nil
}
