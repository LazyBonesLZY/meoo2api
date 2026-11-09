package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"meoo2api/internal/model"
	"meoo2api/internal/upstream"
)

// readSSE handles CRLF, multiline data, comments, and events larger than the
// default Scanner token. A bounded event limit prevents unbounded allocations.
func readSSE(r io.Reader, emit func(string, string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 8192), maxResponseBytes)
	lines := []string{}
	data := []string{}
	size := 0
	dispatch := func() error {
		if len(lines) == 0 {
			return nil
		}
		err := emit(strings.Join(lines, "\n")+"\n\n", strings.Join(data, "\n"))
		lines, data, size = nil, nil, 0
		return err
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		size += len(line)
		if size > maxResponseBytes {
			return fmt.Errorf("upstream SSE event exceeds size limit")
		}
		lines = append(lines, line)
		if line == "data" {
			data = append(data, "")
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

func streamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func streamWrite(w http.ResponseWriter, raw string) error {
	if _, err := io.WriteString(w, raw); err != nil {
		return &upstream.Error{Status: 499, Type: "api_error", Code: "request_cancelled", Message: err.Error(), Cause: err}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func dataEvent(v any) string {
	raw, _ := json.Marshal(v)
	return "data: " + string(raw) + "\n\n"
}

type streamTransform func(map[string]any) map[string]any

func (run *callRun) chatStream(w http.ResponseWriter, payload map[string]any, n int, transform streamTransform) {
	ctx, cancel := context.WithCancel(run.r.Context())
	defer cancel()
	// Ask upstream for accounting independently of the client's preference.
	includeUsage := false
	options := map[string]any{"include_usage": true}
	if so, ok := payload["stream_options"].(map[string]any); ok {
		includeUsage, _ = so["include_usage"].(bool)
		for k, v := range so {
			if k != "include_usage" {
				options[k] = v
			}
		}
	}
	payload["stream_options"] = options
	type opened struct {
		i    int
		resp *http.Response
		err  error
	}
	ready := make(chan opened, n)
	for i := 0; i < n; i++ {
		go func(index int) {
			resp, err := run.h.Up.Chat(ctx, payload, run.key, run.h.timeout(), true)
			if err == nil && !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
				resp.Body.Close()
				err = fmt.Errorf("上游未返回 text/event-stream")
			}
			ready <- opened{index, resp, err}
		}(i)
	}
	responses := make([]*http.Response, n)
	var firstErr error
	for i := 0; i < n; i++ {
		v := <-ready
		if v.err != nil && firstErr == nil {
			firstErr = v.err
			cancel()
		}
		responses[v.i] = v.resp
	}
	defer func() {
		for _, resp := range responses {
			if resp != nil {
				resp.Body.Close()
			}
		}
	}()
	if firstErr != nil {
		run.finish(firstErr, 0, 0)
		respondError(w, firstErr)
		return
	}
	type event struct {
		i         int
		raw, data string
		err       error
		end       bool
	}
	ch := make(chan event, n)
	for i, resp := range responses {
		go func(index int, resp *http.Response) {
			done := false
			err := readSSE(resp.Body, func(raw, data string) error {
				if strings.TrimSpace(data) == "[DONE]" {
					done = true
					return io.EOF
				}
				select {
				case ch <- event{i: index, raw: raw, data: data}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			if done && errors.Is(err, io.EOF) {
				err = nil
			} else if err == nil && !done {
				err = fmt.Errorf("上游流在 [DONE] 前结束")
			}
			resp.Body.Close()
			ch <- event{i: index, err: err, end: true}
		}(i, resp)
	}
	streamHeaders(w)
	var pt, ct int64
	usagePT, usageCT := make([]int64, n), make([]int64, n)
	// [DONE] only terminates the transport. Every choice must also have a
	// terminal finish_reason, otherwise a partial answer would look successful.
	terminal := make([]map[int]bool, n)
	for i := range terminal {
		terminal[i] = map[int]bool{}
	}
	metadata := map[string]any{"object": "chat.completion.chunk", "model": run.model}
	if n > 1 {
		metadata["id"] = model.RandID("chatcmpl-", 8)
		metadata["created"] = run.start.Unix()
	}
	for remaining := n; remaining > 0; {
		e := <-ch
		if e.end {
			remaining--
			if e.err == nil && firstErr == nil {
				complete := len(terminal[e.i]) > 0
				for _, finished := range terminal[e.i] {
					complete = complete && finished
				}
				if !complete {
					e.err = fmt.Errorf("上游流在 choice finish_reason 之前发送 [DONE]")
				}
			}
			if e.err != nil && firstErr == nil {
				firstErr = e.err
				cancel()
			}
			continue
		}
		if firstErr != nil {
			continue
		}
		if strings.TrimSpace(e.data) == "" {
			if n == 1 && transform == nil {
				firstErr = streamWrite(w, e.raw)
				if firstErr != nil {
					cancel()
				}
			}
			continue
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(e.data), &d); err != nil || d == nil {
			firstErr = fmt.Errorf("上游 SSE data 不是 JSON 对象")
			cancel()
			continue
		}
		if v, ok := d["error"].(map[string]any); ok {
			msg, _ := v["message"].(string)
			firstErr = &upstream.Error{Status: 502, Type: "api_error", Code: "upstream_stream_error", Message: msg}
			cancel()
			continue
		}
		for _, k := range []string{"id", "created", "model", "system_fingerprint"} {
			if n > 1 && (k == "id" || k == "created" || k == "model") {
				d[k] = metadata[k]
				continue
			}
			if v, ok := d[k]; ok {
				metadata[k] = v
			}
		}
		if _, ok := d["usage"].(map[string]any); ok {
			p, c := tokens(d)
			pt += p - usagePT[e.i]
			ct += c - usageCT[e.i]
			usagePT[e.i], usageCT[e.i] = p, c
		}
		choices, _ := d["choices"].([]any)
		for _, choice := range choices {
			m, ok := choice.(map[string]any)
			if !ok {
				firstErr = fmt.Errorf("上游 SSE choice 格式错误")
				break
			}
			index, ok := m["index"].(float64)
			if !ok || index < 0 || index != float64(int(index)) {
				firstErr = fmt.Errorf("上游 SSE choice index 格式错误")
				break
			}
			idx := int(index)
			if _, seen := terminal[e.i][idx]; !seen {
				terminal[e.i][idx] = false
			}
			if reason, ok := m["finish_reason"].(string); ok && strings.TrimSpace(reason) != "" {
				terminal[e.i][idx] = true
			}
		}
		if firstErr != nil {
			cancel()
			continue
		}
		if n > 1 {
			for _, choice := range choices {
				if m, ok := choice.(map[string]any); ok {
					m["index"] = e.i
				}
			}
		}
		if _, ok := d["usage"]; ok && (n > 1 || !includeUsage) {
			delete(d, "usage")
			if len(choices) == 0 {
				continue
			}
		}
		if transform != nil {
			d = transform(d)
		}
		firstErr = streamWrite(w, dataEvent(d))
		if firstErr != nil {
			cancel()
		}
	}
	if firstErr == nil && n > 1 && includeUsage {
		metadata["choices"] = []any{}
		metadata["usage"] = map[string]any{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": pt + ct}
		if transform != nil {
			metadata = transform(metadata)
		}
		firstErr = streamWrite(w, dataEvent(metadata))
	}
	if firstErr != nil {
		if errors.Is(firstErr, context.DeadlineExceeded) && run.r.Context().Err() == nil {
			firstErr = &upstream.Error{Status: 504, Type: "api_error", Code: "upstream_timeout", Message: "上游超时", Cause: firstErr}
		}
		e := asError(firstErr)
		if run.key != "" {
			e.Message = strings.ReplaceAll(e.Message, run.key, "[redacted]")
		}
		if e.Status != 499 && run.r.Context().Err() == nil {
			_ = streamWrite(w, dataEvent(map[string]any{"error": map[string]any{"message": e.Message, "type": e.Type, "code": e.Code}}))
		}
	} else {
		firstErr = streamWrite(w, "data: [DONE]\n\n")
	}
	run.finish(firstErr, pt, ct)
}
