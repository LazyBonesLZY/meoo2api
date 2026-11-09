package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

var apiBase = "https://api.meoo.host/meoo-ai/compatible-mode/v1"

type UpErr struct {
	Status  int
	Type    string
	Code    string
	Message string
}

func (e *UpErr) Error() string { return e.Message }

var httpClient = &http.Client{Transport: &http.Transport{
	MaxIdleConns:        100,
	MaxIdleConnsPerHost: 50,
	IdleConnTimeout:     90 * time.Second,
	ForceAttemptHTTP2:   true,
}}

// Call 向 Meoo 网关发起请求，成功时返回原始响应（调用方负责关闭 Body）
func Call(ctx context.Context, path string, payload any, key string, timeout time.Duration, stream bool) (*http.Response, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, &UpErr{Status: 400, Type: "invalid_request_error", Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+path, bytes.NewReader(body))
	if err != nil {
		cancel()
		return nil, &UpErr{Status: 500, Type: "api_error", Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, &UpErr{Status: 502, Type: "api_error", Code: "upstream_timeout", Message: "上游超时"}
		}
		return nil, &UpErr{Status: 502, Type: "api_error", Code: "upstream_unreachable", Message: err.Error()}
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 调用方读取完 body 后 cancel（通过 wrapBody 处理）
		resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
		return resp, nil
	}
	defer cancel()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	up := &UpErr{Status: resp.StatusCode, Type: "invalid_request_error"}
	var parsed struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		up.Message = parsed.Error.Message
		floatType(up, &parsed.Error.Type, &parsed.Error.Code)
	} else {
		up.Message = string(raw)
		if up.Message == "" {
			up.Message = fmt.Sprintf("上游 HTTP %d", resp.StatusCode)
		}
	}
	return nil, up
}

func floatType(up *UpErr, typ, code *string) {
	if *typ != "" {
		up.Type = *typ
	}
	up.Code = *code
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
