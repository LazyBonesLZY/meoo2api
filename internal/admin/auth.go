package admin

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const sessionLifetime = 12 * time.Hour

type rateWindow struct {
	start time.Time
	count int
}

type Tokens struct {
	mu     sync.Mutex
	data   map[string]time.Time
	limits map[string]rateWindow
}

func NewTokens() *Tokens {
	return &Tokens{data: make(map[string]time.Time), limits: make(map[string]rateWindow)}
}

func (t *Tokens) issue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.prune(now)
	// Bound memory even if a client repeatedly authenticates without logging out.
	if len(t.data) >= 256 {
		var oldest string
		var expiry time.Time
		for key, exp := range t.data {
			if oldest == "" || exp.Before(expiry) {
				oldest, expiry = key, exp
			}
		}
		delete(t.data, oldest)
	}
	t.data[tok] = now.Add(sessionLifetime)
	return tok, nil
}

func bearer(r *http.Request) string {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || parts[0] != "Bearer" || len(parts[1]) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return ""
	}
	return parts[1]
}

func (t *Tokens) valid(r *http.Request) bool {
	tok := bearer(r)
	t.mu.Lock()
	defer t.mu.Unlock()
	exp, ok := t.data[tok]
	if !ok || !time.Now().Before(exp) {
		delete(t.data, tok)
		return false
	}
	return true
}

func (t *Tokens) revoke(r *http.Request) {
	t.mu.Lock()
	delete(t.data, bearer(r))
	t.mu.Unlock()
}

func (t *Tokens) Clear() {
	t.mu.Lock()
	t.data = make(map[string]time.Time)
	t.mu.Unlock()
}

func (t *Tokens) prune(now time.Time) {
	for key, expiry := range t.data {
		if !now.Before(expiry) {
			delete(t.data, key)
		}
	}
	for key, window := range t.limits {
		if now.Sub(window.start) >= time.Minute {
			delete(t.limits, key)
		}
	}
}

func (t *Tokens) allow(key string, max int) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.prune(now)
	v := t.limits[key]
	if v.start.IsZero() {
		if len(t.limits) >= 2048 {
			return false
		}
		v.start = now
	}
	if v.count >= max {
		return false
	}
	v.count++
	t.limits[key] = v
	return true
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

// No ambient cookie authentication or CORS is used. Browser requests must be
// same-origin, and every authenticated request requires the explicit bearer.
// Forwarded headers are deliberately not trusted; reverse proxies should retain
// Host and can omit Origin for non-browser clients, never rewrite foreign Origin.
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	// HTTPS-terminating proxies present an HTTP request locally; comparing host
	// still enforces the browser boundary while supporting this common deployment.
	return strings.EqualFold(u.Host, r.Host) && (r.TLS == nil || u.Scheme == "https")
}

func rateLimited(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	write(w, http.StatusTooManyRequests, map[string]any{"error": "请求过于频繁，请稍后重试"})
}
