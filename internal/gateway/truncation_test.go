package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These regressions distinguish generation limits from transport truncation.
// All requests stay in local mock servers; no completion uses a real account.
func TestTruncationUnspecifiedOutputLimitFollowsUpstream(t *testing.T) {
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"messages":[{"role":"user","content":"long answer"}]}`},
		{"/v1/completions", `{"prompt":"long answer"}`},
		{"/v1/responses", `{"input":"long answer"}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := make(chan map[string]any, 1)
			h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				json.NewDecoder(r.Body).Decode(&p)
				got <- p
				completion(w, "full answer")
			})
			h.St.Data.Config.DefaultMaxTokens = 0
			w := request(h, tc.path, tc.body)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			p := <-got
			if cap, exists := p["max_tokens"]; exists {
				t.Fatalf("omitted client limit was silently capped at %v tokens", cap)
			}
		})
	}
}

func TestTruncationExplicitOutputLimitsArePreserved(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		want             float64
	}{
		{"chat max_tokens", "/v1/chat/completions", `{"messages":[{"role":"user"}],"max_tokens":1234}`, 1234},
		{"chat max_completion_tokens", "/v1/chat/completions", `{"messages":[{"role":"user"}],"max_completion_tokens":2345}`, 2345},
		{"chat both prefers max_tokens", "/v1/chat/completions", `{"messages":[{"role":"user"}],"max_tokens":3456,"max_completion_tokens":100}`, 3456},
		{"legacy", "/v1/completions", `{"prompt":"hi","max_tokens":4567}`, 4567},
		{"responses", "/v1/responses", `{"input":"hi","max_output_tokens":5678}`, 5678},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan map[string]any, 1)
			h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				var p map[string]any
				json.NewDecoder(r.Body).Decode(&p)
				got <- p
				completion(w, "full answer")
			})
			h.St.Data.Config.DefaultMaxTokens = 0
			w := request(h, tc.path, tc.body)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			p := <-got
			if p["max_tokens"] != tc.want || p["max_completion_tokens"] != nil || p["max_output_tokens"] != nil {
				t.Fatalf("explicit token limit lost: %v", p)
			}
		})
	}
}

func truncationFrame(w io.Writer, text string, reason any) {
	io.WriteString(w, dataEvent(map[string]any{"id": "chatcmpl-truncation", "object": "chat.completion.chunk", "created": 1,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": reason}}}))
}

func TestTruncationLengthRemainsVisibleAcrossAllAPIs(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, api := range []string{"chat", "legacy", "responses"} {
			t.Run(fmt.Sprintf("%s/stream=%v", api, stream), func(t *testing.T) {
				h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						truncationFrame(w, "unfinished sentence", nil)
						truncationFrame(w, "", "length")
						io.WriteString(w, "data: [DONE]\n\n")
						return
					}
					io.WriteString(w, `{"id":"c","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"unfinished sentence"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":800,"total_tokens":810}}`)
				})
				path, body := "/v1/chat/completions", fmt.Sprintf(`{"messages":[{"role":"user"}],"stream":%v}`, stream)
				if api == "legacy" {
					path = "/v1/completions"
					body = fmt.Sprintf(`{"prompt":"hi","stream":%v}`, stream)
				}
				if api == "responses" {
					path = "/v1/responses"
					body = fmt.Sprintf(`{"input":"hi","stream":%v}`, stream)
				}
				w := request(h, path, body)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
				if api == "responses" {
					if !stream {
						d := decodeBody(t, w)
						if d["status"] != "incomplete" || d["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
							t.Fatal("length became completed", d)
						}
					} else if !strings.Contains(w.Body.String(), "event: response.incomplete") || strings.Contains(w.Body.String(), "event: response.completed") {
						t.Fatal("length became completed", w.Body.String())
					}
				} else if !strings.Contains(w.Body.String(), `"finish_reason":"length"`) {
					t.Fatal("length reason was dropped", w.Body.String())
				}
			})
		}
	}
}

func TestTruncationPrematureDONEIsNotSuccessful(t *testing.T) {
	for _, api := range []string{"chat", "legacy", "responses"} {
		for _, pattern := range []string{"done only", "partial then done", "finish after done", "heartbeat only", "second unfinished choice"} {
			t.Run(api+"/"+pattern, func(t *testing.T) {
				h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if pattern == "heartbeat only" {
						io.WriteString(w, ": keepalive\n\n")
					} else if pattern != "done only" {
						truncationFrame(w, "first half", nil)
					}
					if pattern == "second unfinished choice" {
						truncationFrame(w, "", "stop")
						io.WriteString(w, dataEvent(map[string]any{"choices": []any{map[string]any{"index": 1, "delta": map[string]any{"content": "unfinished second choice"}, "finish_reason": nil}}}))
					}
					io.WriteString(w, "data: [DONE]\n\n")
					if pattern == "finish after done" {
						truncationFrame(w, "ignored second half", "stop")
					}
				})
				path, body := "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`
				if api == "legacy" {
					path = "/v1/completions"
					body = `{"prompt":"hi","stream":true}`
				}
				if api == "responses" {
					path = "/v1/responses"
					body = `{"input":"hi","stream":true}`
				}
				w := request(h, path, body)
				if w.Code != 200 {
					t.Fatal("stream headers unexpectedly changed", w.Code)
				}
				if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "data: [DONE]\n\n") || strings.Contains(w.Body.String(), "event: response.completed") {
					t.Errorf("premature DONE reported as success: %s", w.Body.String())
				}
				if api == "responses" && !strings.Contains(w.Body.String(), "event: response.failed") {
					t.Error("Responses did not emit failure")
				}
				if log := h.St.Data.Logs[0]; log.Status != 502 || k.Stats.OK != 0 {
					t.Errorf("truncated stream counted as success: log=%+v key=%+v", log, k.Stats)
				}
			})
		}
	}
}

func TestTruncationFanoutRejectsAnyUnfinishedStream(t *testing.T) {
	var calls atomic.Int32
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			truncationFrame(w, "complete", "stop")
		} else {
			truncationFrame(w, "partial", nil)
		}
		io.WriteString(w, "data: [DONE]\n\n")
	})
	w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true,"n":2}`)
	if !strings.Contains(w.Body.String(), `"error"`) || strings.Contains(w.Body.String(), "data: [DONE]\n\n") || k.Stats.OK != 0 || h.St.Data.Logs[0].Status != 502 {
		t.Fatal("one successful choice hid unfinished sibling", w.Body.String(), k.Stats)
	}
}

func TestTruncationLongEventsAndTotalStreamSize(t *testing.T) {
	t.Run("single event beyond scanner default", func(t *testing.T) {
		text := strings.Repeat("界", 128<<10)
		frame := dataEvent(map[string]any{"text": text})
		var got string
		if err := readSSE(strings.NewReader(frame), func(_ string, data string) error {
			var d map[string]string
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return err
			}
			got = d["text"]
			return nil
		}); err != nil || got != text {
			t.Fatalf("large valid event truncated: got=%d want=%d err=%v", len(got), len(text), err)
		}
	})
	t.Run("total stream can exceed 32MiB", func(t *testing.T) {
		frame := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
		readers := make([]io.Reader, 34)
		for i := range readers {
			readers[i] = strings.NewReader(frame)
		}
		count := 0
		err := readSSE(io.MultiReader(readers...), func(_ string, data string) error {
			count++
			if len(data) != (1 << 20) {
				return fmt.Errorf("truncated event %d", len(data))
			}
			return nil
		})
		if err != nil || count != 34 {
			t.Fatalf("total stream limited: events=%d err=%v", count, err)
		}
	})
	t.Run("single event over 32MiB errors not silent clipping", func(t *testing.T) {
		emitted := false
		err := readSSE(strings.NewReader("data: "+strings.Repeat("x", maxResponseBytes)+"\n\n"), func(string, string) error { emitted = true; return nil })
		if err == nil || emitted {
			t.Fatal("oversized SSE event silently accepted or partially emitted", err, emitted)
		}
	})
}

func TestTruncationInterleavedToolArgumentsAndLargeText(t *testing.T) {
	for _, api := range []string{"chat", "responses"} {
		t.Run(api, func(t *testing.T) {
			longText := strings.Repeat("answer ", 16000)
			longArgs := strings.Repeat("x", 160<<10)
			h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				truncationFrame(w, longText, nil)
				for _, calls := range [][]any{
					{map[string]any{"index": 0, "id": "call_a", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\""}}, map[string]any{"index": 1, "id": "call_b", "type": "function", "function": map[string]any{"name": "clock", "arguments": "{\"zone\":\""}}},
					{map[string]any{"index": 1, "function": map[string]any{"arguments": "UTC\"}"}}, map[string]any{"index": 0, "function": map[string]any{"arguments": longArgs}}},
					{map[string]any{"index": 0, "function": map[string]any{"arguments": "\"}"}}},
				} {
					io.WriteString(w, dataEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}, "finish_reason": nil}}}))
				}
				truncationFrame(w, "", "tool_calls")
				io.WriteString(w, "data: [DONE]\n\n")
			})
			path, body := "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`
			if api == "responses" {
				path = "/v1/responses"
				body = `{"input":"hi","stream":true}`
			}
			w := request(h, path, body)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if api == "chat" {
				var text strings.Builder
				args := map[int]string{}
				for _, d := range sseData(t, w) {
					cs, _ := d["choices"].([]any)
					for _, c := range cs {
						delta, _ := c.(map[string]any)["delta"].(map[string]any)
						if v, ok := delta["content"].(string); ok {
							text.WriteString(v)
						}
						if calls, ok := delta["tool_calls"].([]any); ok {
							for _, call := range calls {
								m := call.(map[string]any)
								f := m["function"].(map[string]any)
								v, _ := f["arguments"].(string)
								args[int(m["index"].(float64))] += v
							}
						}
					}
				}
				if text.String() != longText || args[0] != "{\"q\":\""+longArgs+"\"}" || args[1] != "{\"zone\":\"UTC\"}" {
					t.Fatal("chat stream lost text or interleaved arguments")
				}
			} else {
				found := false
				for _, d := range sseData(t, w) {
					if d["type"] == "response.completed" {
						found = true
						resp := d["response"].(map[string]any)
						if resp["output_text"] != longText {
							t.Fatal("Responses text truncated")
						}
						calls := map[string]string{}
						for _, v := range resp["output"].([]any) {
							m := v.(map[string]any)
							if m["type"] == "function_call" {
								calls[m["call_id"].(string)] = m["arguments"].(string)
							}
						}
						if calls["call_a"] != "{\"q\":\""+longArgs+"\"}" || calls["call_b"] != "{\"zone\":\"UTC\"}" {
							t.Fatal("Responses tool arguments truncated")
						}
					}
				}
				if !found {
					t.Fatal("no completed response")
				}
			}
		})
	}
}

type truncationRoundTripper func(*http.Request) (*http.Response, error)

func (f truncationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTruncationTimeoutCoversEntireActiveStream(t *testing.T) {
	t.Run("configured180sAbsoluteDeadline", func(t *testing.T) {
		h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected real transport") })
		h.St.Data.Config.TimeoutMs = 180000
		var remaining time.Duration
		h.Up.HTTP.Transport = truncationRoundTripper(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Error("no upstream deadline")
			}
			remaining = time.Until(deadline)
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(dataEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}) + "data: [DONE]\n\n"))}, nil
		})
		w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`)
		if w.Code != 200 || remaining < 179*time.Second || remaining > 180*time.Second {
			t.Fatal("unexpected configured deadline", w.Code, remaining)
		}
	})
	t.Run("activityDoesNotResetTotalDeadline", func(t *testing.T) {
		h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			w.Header().Set("Content-Type", "text/event-stream")
			truncationFrame(w, "partial", nil)
			w.(http.Flusher).Flush()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			bound, cancel := context.WithTimeout(r.Context(), time.Second)
			defer cancel()
			for {
				select {
				case <-bound.Done():
					return
				case <-ticker.C:
					truncationFrame(w, " continuing", nil)
					w.(http.Flusher).Flush()
				}
			}
		})
		h.St.Data.Config.TimeoutMs = 50
		w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`)
		if w.Code != 200 || !strings.Contains(w.Body.String(), "partial") || !strings.Contains(w.Body.String(), "upstream_timeout") || strings.Contains(w.Body.String(), "data: [DONE]\n\n") {
			t.Fatal("active stream timeout silently succeeded", w.Code, w.Body.String())
		}
		if h.St.Data.Logs[0].Status != 504 || k.Stats.OK != 0 {
			t.Fatal("timeout counted success", h.St.Data.Logs[0], k.Stats)
		}
	})
}
