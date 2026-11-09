// Package upstream 封装对 Meoo OpenAI 兼容网关的调用。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
}

func (e *Error) Error() string { return e.Message }

func New(base string) *Client {
	return &Client{Base: base, HTTP: &http.Client{Transport: &http.Transport{
		MaxIdleConns: 100, MaxIdleConnsPerHost: 50, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true,
	}}}
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelBody) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// Chat 调用 /chat/completions，成功返回原始响应由调用方关闭
func (c *Client) Chat(ctx context.Context, payload any, key string, timeout time.Duration, stream bool) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &Error{Status: 400, Type: "invalid_request_error", Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &Error{Status: 500, Type: "api_error", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, &Error{Status: 502, Type: "api_error", Code: "upstream_timeout", Message: "上游超时"}
		}
		return nil, &Error{Status: 502, Type: "api_error", Code: "upstream_unreachable", Message: err.Error()}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	defer cancel()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	up := &Error{Status: resp.StatusCode, Type: "invalid_request_error"}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		up.Message = parsed.Error.Message
		if parsed.Error.Type != "" {
			up.Type = parsed.Error.Type
		}
		up.Code = parsed.Error.Code
	} else if up.Message = string(raw); up.Message == "" {
		up.Message = fmt.Sprintf("上游 HTTP %d", resp.StatusCode)
	}
	return nil, up
}

// DoJSON 发送任意 JSON 请求（用于 meoo.com 的 Open API / 网页接口）
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}
