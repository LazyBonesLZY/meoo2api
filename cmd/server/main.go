// Command server starts the OpenAI-compatible gateway and account console.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/admin"
	"meoo2api/internal/catalog"
	"meoo2api/internal/checkin"
	"meoo2api/internal/gateway"
	"meoo2api/internal/pool"
	"meoo2api/internal/store"
	"meoo2api/internal/upstream"
	"meoo2api/internal/webui"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	port := flag.Int("port", atoi(env("PORT", "8791")), "监听端口")
	bind := flag.String("bind", env("BIND", "127.0.0.1"), "监听地址")
	dataDir := flag.String("data", env("DATA_DIR", "data"), "数据目录")
	reset := flag.Bool("reset-password", false, "重置管理端密码并退出（先停止服务）")
	flag.Parse()
	if *port < 1 || *port > 65535 {
		return fmt.Errorf("端口必须在 1 到 65535 之间")
	}

	base := strings.TrimRight(env("MEOO_AI_BASE", "https://api.meoo.host/meoo-ai/compatible-mode/v1"), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("MEOO_AI_BASE 必须为有效的 HTTP(S) 基础地址")
	}
	st, generated, err := store.Load(*dataDir, os.Getenv("MEOO_PROJECT_AK"), splitList(os.Getenv("MEOO2API_KEYS")))
	if err != nil {
		return fmt.Errorf("加载状态: %w", err)
	}
	defer st.Close()
	if *reset {
		pw := randomPassword()
		st.Mu.Lock()
		st.Data.Config.AdminPass = hashPassword(pw)
		st.Mu.Unlock()
		if err := st.Save(); err != nil {
			return err
		}
		fmt.Printf("新的管理端密码（只显示这一次）：%s\n", pw)
		return nil
	}
	if generated != "" {
		fmt.Printf("管理端初始密码（只显示这一次）：%s\n", generated)
		fmt.Println("重置方式：先停止服务，再执行 meoo2api --reset-password")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	up := upstream.New(base)
	defer up.HTTP.CloseIdleConnections()
	acc := &account.Service{St: st, Up: up}
	picker := &pool.Picker{St: st, Up: up}
	models := catalog.New(st, up)
	gw := &gateway.Handler{St: st, Picker: picker, Up: up, Catalog: models}
	adm := &admin.Handler{St: st, Picker: picker, Acc: acc, Tokens: admin.NewTokens(), Start: time.Now(), Catalog: models}
	catalogDone := models.Start(ctx)
	var saveWG sync.WaitGroup
	saveWG.Add(1)
	go func() { defer saveWG.Done(); st.SaveLoopContext(ctx) }()
	schedulerDone := checkin.StartSchedulerWithContext(ctx, st, acc.PingContext)
	srv := &http.Server{
		Addr:              net.JoinHostPort(*bind, strconv.Itoa(*port)),
		Handler:           newHandler(st, picker, gw, adm, base),
		ReadHeaderTimeout: 15 * time.Second, ReadTimeout: 60 * time.Second,
		IdleTimeout: 120 * time.Second, MaxHeaderBytes: 1 << 20,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()
	log.Printf("meoo2api listening on %s", srv.Addr)
	select {
	case <-ctx.Done():
	case err = <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		_ = srv.Close()
		log.Printf("HTTP shutdown: %v", shutdownErr)
	}
	<-schedulerDone
	<-catalogDone
	saveWG.Wait()
	if saveErr := st.Save(); saveErr != nil {
		return fmt.Errorf("保存最终状态: %w", saveErr)
	}
	return err
}

func newHandler(st *store.Store, picker *pool.Picker, gw *gateway.Handler, adm *admin.Handler, base string) http.Handler {
	html := webui.Index()
	return http.HandlerFunc(func(raw http.ResponseWriter, r *http.Request) {
		w := &stateWriter{ResponseWriter: raw}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("HTTP panic: %v", recovered)
				if w.wrote {
					panic(http.ErrAbortHandler)
				}
				writeError(w, http.StatusInternalServerError, "internal_error", "内部错误")
			}
		}()
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		path := strings.TrimRight(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		if !strings.HasPrefix(path, "/admin/") {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		switch {
		case path == "/" || path == "/webui.html" || path == "/index.html":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeError(w, 405, "method_not_allowed", "方法不支持")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
			if r.Method != http.MethodHead {
				_, _ = w.Write(html)
			}
		case path == "/healthz":
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				writeError(w, 405, "method_not_allowed", "方法不支持")
				return
			}
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				return
			}
			health(w, st, picker, base)
		default:
			if adm.ServeHTTP(w, r) {
				return
			}
			if isGatewayPath(path) {
				gw.ServeHTTP(w, r)
				return
			}
			writeError(w, 404, "unknown_url", "未知路径")
		}
	})
}

func isGatewayPath(path string) bool {
	if strings.HasPrefix(path, "/v1/") {
		return true
	}
	for _, prefix := range []string{"/models", "/chat", "/completions", "/responses", "/embeddings", "/audio", "/images"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "invalid_request_error", "code": code}})
}

func health(w http.ResponseWriter, st *store.Store, picker *pool.Picker, base string) {
	st.Mu.RLock()
	total, accounts := len(st.Data.Keys), len(st.Data.Accounts)
	st.Mu.RUnlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "upstream": base, "keys": total, "usable": len(picker.Usable()),
		"accounts": accounts, "alerts": st.Alerts(),
	})
}

const maxBodyBytes = 8 << 20

type stateWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *stateWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.ResponseWriter.WriteHeader(code)
}
func (w *stateWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *stateWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *stateWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 8791
	}
	return n
}
