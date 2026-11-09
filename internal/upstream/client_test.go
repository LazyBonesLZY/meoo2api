package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestChatRequestAndErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"success", `{"choices":[]}`, "", 200},
		{"rate", `{"error":{"message":"slow","type":"rate_limit_error","code":"quota"}}`, "quota", 429},
		{"numeric", `{"error":{"message":"bad","code":123}}`, "123", 400},
		{"null", `{"error":{"message":"bad","code":null}}`, "", 403},
		{"plain", "outage", "", 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer secret" || r.Header.Get("Accept") != "application/json" {
					t.Errorf("unexpected request %s %s %v", r.Method, r.URL, r.Header)
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			client := New(srv.URL + "/v1/")
			resp, err := client.Chat(context.Background(), map[string]any{"messages": []any{}}, "secret", time.Second, false)
			if tc.status == 200 {
				if err != nil {
					t.Fatal(err)
				}
				raw, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || string(raw) != tc.body {
					t.Fatalf("read %q %v", raw, err)
				}
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Status != tc.status || e.Code != tc.code {
					t.Fatalf("error = %#v", err)
				}
			}
		})
	}
}

func TestChatCancellationAndBodyTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	client := New(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := client.Chat(ctx, nil, "k", time.Second, false)
	var e *Error
	if !errors.As(err, &e) || e.Code != "request_cancelled" || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %#v", err)
	}
	_, err = client.Chat(context.Background(), nil, "k", 20*time.Millisecond, false)
	if !errors.As(err, &e) || e.Status != 504 || e.Code != "upstream_timeout" {
		t.Fatalf("timeout: %#v", err)
	}
	resp, err := client.Chat(context.Background(), nil, "k", 20*time.Millisecond, true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("body timeout: %v", err)
	}
}

func TestNoRedirectCredentialLeak(t *testing.T) {
	var hits atomic.Int32
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer dest.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client := New(source.URL)
	client.HTTP = source.Client() // same policy also applies to supplied clients
	_, err := client.Chat(context.Background(), nil, "secret", time.Second, false)
	var e *Error
	if !errors.As(err, &e) || e.Status != 502 || hits.Load() != 0 {
		t.Fatalf("redirect error=%v hits=%d", err, hits.Load())
	}
	status, _, err := client.DoJSON(context.Background(), "GET", source.URL, nil, nil)
	if err != nil || status != 307 || hits.Load() != 0 {
		t.Fatalf("DoJSON redirect %d %v hits=%d", status, err, hits.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (failingReader) Close() error             { return nil }

func TestDoJSONReadFailuresAndLimit(t *testing.T) {
	client := New("http://fixture")
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: failingReader{}, Header: http.Header{}}, nil
	})
	_, _, err := client.DoJSON(context.Background(), "GET", "http://fixture", nil, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error: %v", err)
	}
	client.HTTP.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1))), Header: http.Header{}}, nil
	})
	_, _, err = client.DoJSON(context.Background(), "GET", "http://fixture", nil, nil)
	if err == nil {
		t.Fatal("oversized response accepted")
	}
}
