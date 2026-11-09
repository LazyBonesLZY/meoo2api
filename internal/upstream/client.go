// Package upstream 封装对 Meoo OpenAI 兼容网关的调用。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Client struct {
	Base string
	HTTP *http.Client
}

type Error struct {
	Status  int
	Type    string
	Code    string
	Message string
	Cause   error
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func New(base string) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 50
	transport.IdleConnTimeout = 90 * time.Second
	return &Client{Base: strings.TrimRight(base, "/"), HTTP: &http.Client{
		Transport: transport,
		// Never forward an API key or a POST payload to a redirect destination.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (c *Client) HTTPClient() *http.Client {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err != nil {
		c.once.Do(c.cancel)
	}
	return n, err
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}

// Chat 调用 /chat/completions；成功响应的生命周期包含读取响应体。
func (c *Client) Chat(parent context.Context, payload any, key string, timeout time.Duration, stream bool) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Status: 400, Type: "invalid_request_error", Message: err.Error(), Cause: err}
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.Base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &Error{Status: 502, Type: "api_error", Code: "upstream_unreachable", Message: err.Error(), Cause: err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		cause := ctx.Err()
		cancel()
		if parent.Err() != nil {
			return nil, &Error{Status: 499, Type: "api_error", Code: "request_cancelled", Message: "请求已取消", Cause: parent.Err()}
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			return nil, &Error{Status: 504, Type: "api_error", Code: "upstream_timeout", Message: "上游超时", Cause: cause}
		}
		return nil, &Error{Status: 502, Type: "api_error", Code: "upstream_unreachable", Message: err.Error(), Cause: err}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	defer cancel()
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	status := resp.StatusCode
	if status < 400 {
		status = http.StatusBadGateway
	}
	up := &Error{Status: status, Type: "api_error"}
	if status >= 400 && status < 500 {
		up.Type = "invalid_request_error"
	}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		up.Message = parsed.Error.Message
		if parsed.Error.Type != "" {
			up.Type = parsed.Error.Type
		}
		if parsed.Error.Code != nil {
			up.Code = fmt.Sprint(parsed.Error.Code)
		}
	} else if up.Message = strings.TrimSpace(string(raw)); up.Message == "" {
		up.Message = fmt.Sprintf("上游 HTTP %d", resp.StatusCode)
	}
	if readErr != nil {
		up.Cause = readErr
	}
	return nil, up
}

// DoJSON 发送任意 JSON 请求（用于 meoo.com 的 Open API / 网页接口）。
func (c *Client) DoJSON(ctx context.Context, method, url string, body any, headers map[string]string) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTPClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	const limit = 1 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, raw, err
	}
	if len(raw) > limit {
		return resp.StatusCode, nil, fmt.Errorf("upstream JSON response exceeds %d bytes", limit)
	}
	return resp.StatusCode, raw, nil
}
