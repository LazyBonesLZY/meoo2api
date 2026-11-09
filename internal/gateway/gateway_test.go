package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"meoo2api/internal/model"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
)

func fixture(t *testing.T, handler http.HandlerFunc) (*Handler, *model.Key) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	st, _, err := store.Load(t.TempDir(), "", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	k := store.NewKey("mock-key", "mock-project-key", 1)
	st.Data.Keys = []*model.Key{k}
	up := upstream.New(srv.URL + "/v1")
	return &Handler{St: st, Picker: &pool.Picker{St: st, Up: up}, Up: up}, k
}

func request(h *Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("status=%d body=%q: %v", w.Code, w.Body.String(), err)
	}
	return d
}
func completion(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-mock", "created": 123, "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}})
}

const chatRequest = `{"messages":[{"role":"user","content":"hi"}]}`

func TestChatDefaultToolsAndAccounting(t *testing.T) {
	var got map[string]any
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer mock-project-key" {
			t.Error("wrong upstream")
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	})
	w := request(h, "/v1/chat/completions", `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_0","type":"function","function":{"name":"weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_0","content":"sunny"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}],"tool_choice":"auto","parallel_tool_calls":true,"max_completion_tokens":42}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if got["model"] != "qwen3.8-flash" || got["max_tokens"] != float64(42) || got["n"] != float64(1) || got["max_completion_tokens"] != nil {
		t.Fatalf("payload=%v", got)
	}
	if got["tools"] == nil || got["parallel_tool_calls"] != true {
		t.Fatal("tools lost")
	}
	d := decodeBody(t, w)
	msg := d["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["tool_calls"] == nil || msg["content"] != nil {
		t.Fatal("tool response lost")
	}
	if k.Stats.Req != 1 || k.Stats.OK != 1 || k.Stats.PT != 2 || k.Stats.CT != 3 {
		t.Fatalf("key stats=%+v", k.Stats)
	}
	if s := h.St.Data.ClientKeys[0].Stats; s.Req != 1 || s.OK != 1 || s.PT != 2 {
		t.Fatalf("client=%+v", s)
	}
	if l := h.St.Data.Logs[0]; l.Model != "qwen3.8-flash" || l.PT != 2 || l.KeyLabel != "mock-key" {
		t.Fatalf("log=%+v", l)
	}
}

func TestValidationMethodsAndAuth(t *testing.T) {
	var hits atomic.Int32
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { hits.Add(1); completion(w, "ok") })
	for _, body := range []string{"null", "[]", chatRequest + ` {}`, `{"model":null,"messages":[{"role":"user"}]}`, `{"messages":[]}`, `{"messages":[null]}`, `{"messages":[{}]}`, `{"messages":[{"role":"user"}],"n":0}`, `{"messages":[{"role":"user"}],"n":1.5}`, `{"messages":[{"role":"user"}],"n":5}`, `{"messages":[{"role":"user"}],"stream":"true"}`, `{"messages":[{"role":"user"}],"max_tokens":-1}`} {
		if w := request(h, "/v1/chat/completions", body); w.Code != 400 {
			t.Errorf("body=%s status=%d %s", body, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/completions", "/v1/responses", "/v1/models"} {
		r := httptest.NewRequest("DELETE", path, nil)
		r.Header.Set("Authorization", "Bearer x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 405 || w.Header().Get("Allow") == "" {
			t.Fatalf("method %s=%d", path, w.Code)
		}
	}
	for _, auth := range []string{"x", "Basic x", "Bearer", "Bearer x extra"} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest))
		r.Header.Set("Authorization", auth)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("auth %q=%d", auth, w.Code)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("invalid requests hit upstream %d", hits.Load())
	}
	large := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBytes) + `"}]}`
	if w := request(h, "/v1/chat/completions", large); w.Code != 413 {
		t.Fatal("large", w.Code)
	}
	if w := request(h, "/v1/chat/completions", chatRequest+strings.Repeat(" ", maxRequestBytes)); w.Code != 413 {
		t.Fatal("large tail", w.Code)
	}
}

func TestErrorsMalformedAndNoUnrelatedCooldown(t *testing.T) {
	for _, tc := range []struct {
		status   int
		body     string
		want     int
		cooldown bool
		code     string
	}{
		{400, `{"error":{"message":"bad request","code":"bad_param"}}`, 400, false, "bad_param"},
		{401, `{"error":{"message":"bad key","code":"bad_key"}}`, 401, true, "bad_key"},
		{429, `{"error":{"message":"quota","type":"rate_limit_error","code":"quota"}}`, 429, true, "quota"},
		{200, `not-json`, 502, true, "invalid_upstream_response"},
		{200, `null`, 502, true, "invalid_upstream_response"},
		{200, `{"choices":[]}`, 502, true, "invalid_upstream_response"},
		{200, `{"choices":[{}]}`, 502, true, "invalid_upstream_response"},
	} {
		t.Run(fmt.Sprint(tc.status, tc.body), func(t *testing.T) {
			h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); io.WriteString(w, tc.body) })
			w := request(h, "/v1/chat/completions", chatRequest)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
			if (k.CooldownUntil > time.Now().UnixMilli()) != tc.cooldown {
				t.Fatalf("cooldown=%d", k.CooldownUntil)
			}
			if got := decodeBody(t, w)["error"].(map[string]any)["code"]; got != tc.code {
				t.Fatalf("code=%v", got)
			}
			if k.Stats.Fail != 1 || h.St.Data.Logs[0].Status != tc.want {
				t.Fatal("failure accounting")
			}
		})
	}
}

func TestNCompletionAggregatesAndCancels(t *testing.T) {
	var hits atomic.Int32
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		if p["n"] != float64(1) {
			t.Error("upstream n!=1")
		}
		hits.Add(1)
		completion(w, "ok")
	})
	w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}],"n":3}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d := decodeBody(t, w)
	cs := d["choices"].([]any)
	if len(cs) != 3 || hits.Load() != 3 {
		t.Fatal("n not honored")
	}
	for i, c := range cs {
		if c.(map[string]any)["index"] != float64(i) {
			t.Fatal("bad index")
		}
	}
	if u := d["usage"].(map[string]any); u["prompt_tokens"] != float64(6) || u["completion_tokens"] != float64(9) {
		t.Fatalf("usage=%v", u)
	}
	if k.Stats.PT != 6 || k.Stats.CT != 9 || k.Stats.OK != 1 {
		t.Fatalf("stats=%+v", k.Stats)
	}
	var calls atomic.Int32
	allStarted := make(chan struct{})
	cancelled := make(chan struct{}, 1)
	h2, k2 := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		i := calls.Add(1)
		if i == 2 {
			close(allStarted)
		}
		if i == 1 {
			<-allStarted
			w.WriteHeader(429)
			io.WriteString(w, `{"error":{"message":"quota","code":"quota"}}`)
			return
		}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-time.After(2 * time.Second):
		}

	})
	w = request(h2, "/v1/chat/completions", `{"messages":[{"role":"user"}],"n":2}`)
	if w.Code != 429 || k2.Stats.Fail != 1 {
		t.Fatal(w.Code, w.Body.String(), k2.Stats)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("sibling not cancelled")
	}
}

func streamFixture(w http.ResponseWriter, tools bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	if tools {
		io.WriteString(w, "data: {\"id\":\"chatcmpl-s\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{\"}}]},\"finish_reason\":null}]}\r\n\r\n")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"} \"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
	} else {
		io.WriteString(w, "data: {\"id\":\"chatcmpl-s\",\"created\":123,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"},\"finish_reason\":null}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	}
	io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
}

func sseData(t *testing.T, w *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	err := readSSE(strings.NewReader(w.Body.String()), func(raw, data string) error {
		if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(data), &d); err != nil {
			return err
		}
		out = append(out, d)
		return nil
	})
	if err != nil {
		t.Fatal(err, w.Body.String())
	}
	return out
}

func TestStreamNToolsUsage(t *testing.T) {
	for _, n := range []int{1, 3} {
		for _, usage := range []bool{false, true} {
			t.Run(fmt.Sprintf("n%d/usage%v", n, usage), func(t *testing.T) {
				h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
					var p map[string]any
					json.NewDecoder(r.Body).Decode(&p)
					if p["n"] != float64(1) || p["stream_options"].(map[string]any)["include_usage"] != true {
						t.Error("bad stream payload")
					}
					streamFixture(w, true)
				})
				w := request(h, "/v1/chat/completions", fmt.Sprintf(`{"messages":[{"role":"user","content":"hi"}],"stream":true,"n":%d,"stream_options":{"include_usage":%v},"tools":[{"type":"function","function":{"name":"weather"}}]}`, n, usage))
				if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatal(w.Code, w.Body.String())
				}
				events := sseData(t, w)
				indices := map[int]bool{}
				usageCount := 0
				for _, e := range events {
					if u, ok := e["usage"].(map[string]any); ok {
						usageCount++
						if u["prompt_tokens"] != float64(2*n) || u["completion_tokens"] != float64(3*n) {
							t.Fatalf("usage=%v", u)
						}
					}
					if cs, ok := e["choices"].([]any); ok {
						for _, c := range cs {
							m := c.(map[string]any)
							indices[int(m["index"].(float64))] = true
							if delta, ok := m["delta"].(map[string]any); ok && delta["tool_calls"] != nil {
								if delta["tool_calls"].([]any)[0].(map[string]any)["index"] != float64(0) {
									t.Fatal("tool index rewritten")
								}
							}
						}
					}
				}
				if len(indices) != n || (usageCount == 1) != usage || strings.Count(w.Body.String(), "[DONE]") != 1 {
					t.Fatal("invalid aggregate stream", w.Body.String())
				}
				if k.Stats.OK != 1 || k.Stats.PT != int64(2*n) || k.Stats.CT != int64(3*n) {
					t.Fatalf("stats=%+v", k.Stats)
				}
			})
		}
	}
}

func TestStreamReadFailuresAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, ct, body string
		want           int
	}{
		{"not sse", "application/json", `{}`, 502},
		{"truncated", "text/event-stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n", 200},
		{"bad data", "text/event-stream", "data: nope\n\n", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.ct)
				io.WriteString(w, tc.body)
			})
			w := request(h, "/v1/chat/completions", `{"messages":[{"role":"user"}],"stream":true}`)
			if w.Code != tc.want || !strings.Contains(w.Body.String(), "error") || strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatal(w.Code, w.Body.String())
			}
			if k.Stats.Fail != 1 || h.St.Data.Logs[0].Status != 502 {
				t.Fatal("failure not recorded")
			}
		})
	}
	started := make(chan struct{})
	h, k := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(chatRequest)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer x")
	finished := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(finished) }()
	<-started
	cancel()
	<-finished
	if k.CooldownUntil != 0 || k.Stats.Req != 0 || h.St.Data.Logs[0].Status != 499 {
		t.Fatalf("cancel polluted key=%+v log=%+v", k, h.St.Data.Logs[0])
	}
}

func TestLegacyBatchNAndStream(t *testing.T) {
	var promptsMu sync.Mutex
	prompts := []string{}
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		var p map[string]any
		json.NewDecoder(r.Body).Decode(&p)
		msg := p["messages"].([]any)[0].(map[string]any)["content"].(string)
		promptsMu.Lock()
		prompts = append(prompts, msg)
		promptsMu.Unlock()
		if p["stream"] == true {
			streamFixture(w, false)
		} else {
			completion(w, msg)
		}
	})
	w := request(h, "/v1/completions", `{"prompt":["a","b"],"n":2}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d := decodeBody(t, w)
	if d["object"] != "text_completion" || len(d["choices"].([]any)) != 4 || d["usage"].(map[string]any)["total_tokens"] != float64(20) {
		t.Fatal(d)
	}
	if len(prompts) != 4 || strings.Join(prompts, "") == "a\nb" {
		t.Fatal("prompt batch merged", prompts)
	}
	w = request(h, "/v1/completions", `{"prompt":"hi","stream":true}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"text":"你好"`) || strings.Contains(w.Body.String(), `"delta"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, body := range []string{`{"prompt":"hi","echo":true}`, `{"prompt":"hi","logprobs":1}`, `{"prompt":"hi","best_of":4}`, `{"prompt":[1,2]}`} {
		if w := request(h, "/v1/completions", body); w.Code != 400 {
			t.Fatal(body, w.Code)
		}
	}
}

func TestResponsesToolsInputAndStream(t *testing.T) {
	var got map[string]any
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		if got["stream"] == true {
			streamFixture(w, true)
			return
		}
		io.WriteString(w, `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	})
	body := `{"input":[{"role":"user","content":[{"type":"input_text","text":"天气"},{"type":"input_image","image_url":"data:image/png;base64,eA=="}]},{"type":"function_call","call_id":"call_prev","name":"weather","arguments":"{}"},{"type":"function_call_output","call_id":"call_prev","output":"sunny"}],"instructions":"system","tools":[{"type":"function","name":"weather","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"weather"},"max_output_tokens":40,"temperature":0.1}`
	w := request(h, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d := decodeBody(t, w)
	out := d["output"].([]any)
	if len(out) != 1 || out[0].(map[string]any)["type"] != "function_call" || out[0].(map[string]any)["call_id"] != "call_1" {
		t.Fatal(d)
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" || msgs[3].(map[string]any)["tool_call_id"] != "call_prev" {
		t.Fatal("messages", msgs)
	}
	if got["max_tokens"] != float64(40) || got["temperature"] != 0.1 || got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"] != "weather" {
		t.Fatal(got)
	}
	w = request(h, "/v1/responses", `{"input":"hi","stream":true,"tools":[{"type":"function","name":"weather"}]}`)
	if w.Code != 200 || strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "event: response.function_call_arguments.delta") || !strings.Contains(w.Body.String(), "event: response.completed") {
		t.Fatal(w.Code, w.Body.String())
	}
	events := sseData(t, w)
	var responseID string
	for i, e := range events {
		if e["sequence_number"] != float64(i) {
			t.Fatal("sequence", e)
		}
		if e["type"] == "response.created" {
			responseID = e["response"].(map[string]any)["id"].(string)
		}
		if e["type"] == "response.completed" {
			resp := e["response"].(map[string]any)
			if resp["id"] != responseID || resp["usage"].(map[string]any)["total_tokens"] != float64(5) {
				t.Fatal(resp)
			}
			call := resp["output"].([]any)[0].(map[string]any)
			if call["arguments"] != "{} " || call["name"] != "weather" || call["call_id"] != "call_1" {
				t.Fatal(call)
			}
		}
	}
	for _, bad := range []string{`{"input":"hi","previous_response_id":"resp_1"}`, `{"input":"hi","store":true}`, `{"input":"hi","background":true}`, `{"input":"hi","tools":[{"type":"web_search"}]}`, `{"input":[{"type":"message","content":[{"type":"input_file"}]}]}`, `{"input":[]}`} {
		if w := request(h, "/v1/responses", bad); w.Code != 400 {
			t.Fatal(bad, w.Code, w.Body.String())
		}
	}
}

func TestModelsAliasIdentityAndConcurrentConfig(t *testing.T) {
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) { completion(w, "ok") })
	h.St.Data.Config.Aliases = map[string]string{"alias": "qwen3.8-flash"}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				r := httptest.NewRequest("GET", "/v1/models", nil)
				r.Header.Set("Authorization", "bearer x")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Error(w.Code)
				}
				h.St.Mu.Lock()
				h.St.Data.Config.HiddenModels = []string{"qwen3.8-max"}
				h.St.Data.ClientKeys[0].Label = "changed"
				h.St.Mu.Unlock()
			}
		}()
	}
	wg.Wait()
	r := httptest.NewRequest("GET", "/models/alias", nil)
	r.Header.Set("Authorization", "Bearer x")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || decodeBody(t, w)["id"] != "alias" {
		t.Fatal(w.Code, w.Body.String())
	}
}
