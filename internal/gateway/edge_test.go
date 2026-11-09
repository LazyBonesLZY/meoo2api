package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBodyReadTimeoutAccounting(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
				} else {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(time.Second):
				}
			})
			h.St.Data.Config.TimeoutMs = 30
			w := request(h, "/v1/chat/completions", fmt.Sprintf(`{"messages":[{"role":"user"}],"stream":%v}`, stream))
			if !stream && w.Code != 504 {
				t.Fatal(w.Code, w.Body.String())
			}
			if stream && (w.Code != 200 || !strings.Contains(w.Body.String(), "upstream_timeout")) {
				t.Fatal(w.Code, w.Body.String())
			}
			if h.St.Data.Logs[0].Status != 504 || k.Stats.Fail != 1 {
				t.Fatalf("log=%+v key=%+v", h.St.Data.Logs[0], k)
			}
		})
	}
}

func TestRepeatedStreamUsageAndStableFanoutID(t *testing.T) {
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":%q,\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1}}\n\n", r.RemoteAddr)
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
	})
	w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"n":2,"stream":true,"stream_options":{"include_usage":true}}`)
	var id any
	for _, d := range sseData(t, w) {
		if id == nil {
			id = d["id"]
		}
		if d["id"] != id {
			t.Fatal("unstable ID", w.Body.String())
		}
	}
	if k.Stats.PT != 4 || k.Stats.CT != 6 {
		t.Fatalf("double-counted cumulative usage: %+v", k.Stats)
	}
}

func TestResponsesTextStreamAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				if !failure {
					streamFixture(w, false)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
			})
			w := request(h, "/v1/responses", `{"input":"hi","stream":true}`)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			if failure {
				if !strings.Contains(w.Body.String(), "event: response.failed") || strings.Contains(w.Body.String(), "event: response.completed") {
					t.Fatal(w.Body.String())
				}
				return
			}
			if !strings.Contains(w.Body.String(), "event: response.content_part.added") || !strings.Contains(w.Body.String(), "event: response.output_text.done") {
				t.Fatal(w.Body.String())
			}
			for _, d := range sseData(t, w) {
				if d["type"] == "response.completed" {
					resp := d["response"].(map[string]any)
					if resp["output_text"] != "你好" || resp["output"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "你好" {
						t.Fatal(resp)
					}
				}
			}
		})
	}
}

func TestBYOKRedactionRequestIDAndNoPool(t *testing.T) {
	key := "meoo_project_synthetic_fixture"
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, r.Header.Get("Authorization"))
	})
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
	r.Header.Set("Authorization", "Bearer "+key)
	r.Header.Set("X-Request-Id", strings.Repeat("r", 1024))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 401 || strings.Contains(w.Body.String(), key) || strings.Contains(h.St.Data.Logs[0].Error, key) || len(h.St.Data.Logs[0].Rid) != 128 || k.Stats.Req != 0 {
		t.Fatal(w.Code, w.Body.String(), h.St.Data.Logs[0])
	}
}

type failingWriter struct{ header http.Header }

func (w *failingWriter) Header() http.Header       { return w.header }
func (w *failingWriter) WriteHeader(int)           {}
func (w *failingWriter) Write([]byte) (int, error) { return 0, fmt.Errorf("disconnected") }
func (w *failingWriter) Flush()                    {}

func TestDownstreamWriteFailureDoesNotCoolKey(t *testing.T) {
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) { streamFixture(w, false) })
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"messages":[{"role":"user"}],"stream":true}`))
	r.Header.Set("Authorization", "Bearer x")
	h.ServeHTTP(&failingWriter{header: http.Header{}}, r)
	if k.CooldownUntil != 0 || k.Stats.Req != 0 || h.St.Data.Logs[0].Status != 499 {
		t.Fatalf("key=%+v log=%+v", k, h.St.Data.Logs[0])
	}
}

func TestResponseFormatAndUnsupportedParameters(t *testing.T) {
	p, err := responsesPayload(map[string]any{"input": "hi", "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "shape", "schema": map[string]any{"type": "object"}, "strict": true}}})
	if err != nil || p["response_format"].(map[string]any)["json_schema"].(map[string]any)["name"] != "shape" {
		t.Fatal(p, err)
	}
	for _, body := range []map[string]any{{"input": "hi", "n": float64(2)}, {"input": "hi", "reasoning": map[string]any{}}, {"input": "hi", "text": "x"}, {"input": "hi", "text": map[string]any{"verbosity": "low"}}, {"input": "hi", "text": map[string]any{"format": nil}}} {
		if _, err := responsesPayload(body); err == nil {
			t.Fatal("silently accepted", body)
		}
	}
}
