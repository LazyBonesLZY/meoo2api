// Command server 启动 meoo2api：OpenAI 兼容网关 + 管理控制台。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"meoo2api/internal/account"
	"meoo2api/internal/admin"
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
	port := flag.Int("port", atoi(env("PORT", "8791")), "监听端口")
	bind := flag.String("bind", env("BIND", "127.0.0.1"), "监听地址")
	dataDir := flag.String("data", env("DATA_DIR", "data"), "数据目录")
	reset := flag.Bool("reset-password", false, "重置管理端密码并退出")
	flag.Parse()

	base := strings.TrimRight(env("MEOO_AI_BASE", "https://api.meoo.host/meoo-ai/compatible-mode/v1"), "/")
	st, generated, err := store.Load(*dataDir, os.Getenv("MEOO_PROJECT_AK"), splitList(os.Getenv("MEOO2API_KEYS")))
	if err != nil {
		log.Fatalf("加载状态失败: %v", err)
	}
	if *reset {
		pw := randomPassword()
		st.Mu.Lock()
		st.Data.Config.AdminPass = hashPassword(pw)
		st.Mu.Unlock()
		_ = st.Save()
		fmt.Printf("新的管理端密码（只显示这一次）：%s\n", pw)
		return
	}
	if generated != "" {
		fmt.Printf("\n  ── 管理端密码（只显示这一次，不落盘）：%s\n", generated)
		fmt.Printf("     忘记可执行：meoo2api --reset-password\n\n")
	}

	go st.SaveLoop()

	up := upstream.New(base)
	acc := &account.Service{St: st, Up: up}
	picker := &pool.Picker{St: st, Up: up}
	checkin.StartScheduler(st, acc.Ping)
	gw := &gateway.Handler{St: st, Picker: picker, Up: up}
	adm := &admin.Handler{St: st, Picker: picker, Acc: acc,
		Tokens: admin.NewTokens(), Start: time.Now()}
	html := webui.Index()

	inner := http.NewServeMux()
	mux := inner
	handler := func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch {
		case path == "/" || path == "/webui.html" || path == "/index.html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(html)
		case path == "/healthz":
			health(w, st, picker, base)
		default:
			if adm.ServeHTTP(w, r) {
				return
			}
			if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/models") ||
				strings.HasPrefix(path, "/chat") || strings.HasPrefix(path, "/completions") ||
				strings.HasPrefix(path, "/responses") || strings.HasPrefix(path, "/embeddings") {
				gw.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"未知路径","type":"invalid_request_error","code":"unknown_url"}}`))
		}
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handler(&stateWriter{ResponseWriter: w}, r)
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *bind, *port),
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	st.Mu.RLock()
	fmt.Printf("meoo2api  http://%s:%d/\n", *bind, *port)
	fmt.Printf("  upstream %s\n", base)
	fmt.Printf("  accounts %d 个 / API Key %d 个\n", len(st.Data.Accounts), len(st.Data.Keys))
	st.Mu.RUnlock()
	log.Fatal(srv.ListenAndServe())
}

func health(w http.ResponseWriter, st *store.Store, picker *pool.Picker, base string) {
	st.Mu.RLock()
	total, accounts := len(st.Data.Keys), len(st.Data.Accounts)
	st.Mu.RUnlock()
	usable := len(picker.Usable())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "upstream": base, "keys": total, "usable": usable,
		"accounts": accounts, "alerts": st.Alerts(),
	})
}

const maxBodyBytes = 8 << 20 // 8MiB

// headerSent 用 ResponseWriter 包装判断是否已写头
type stateWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *stateWriter) WriteHeader(code int)        { w.wrote = true; w.ResponseWriter.WriteHeader(code) }
func (w *stateWriter) Write(b []byte) (int, error) { w.wrote = true; return w.ResponseWriter.Write(b) }
func (w *stateWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func headerSent(w http.ResponseWriter) bool {
	if sw, ok := w.(*stateWriter); ok {
		return sw.wrote
	}
	return false
}

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
