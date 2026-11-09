package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed webui.html
var webuiFS embed.FS

func main() {
	port := flag.Int("port", parseIntDefault(os.Getenv("PORT"), 8791), "监听端口")
	bind := flag.String("bind", defStr(os.Getenv("BIND"), "127.0.0.1"), "监听地址")
	dataDir := flag.String("data", defStr(os.Getenv("DATA_DIR"), "data"), "数据目录")
	reset := flag.Bool("reset-password", false, "重置管理端密码并退出")
	flag.Parse()

	if v := os.Getenv("MEOO_AI_BASE"); v != "" {
		apiBase = strings.TrimRight(v, "/")
	}

	state, genPass, err := LoadState(*dataDir, os.Getenv("MEOO_PROJECT_AK"), splitList(os.Getenv("MEOO2API_KEYS")))
	if err != nil {
		log.Fatalf("加载状态失败: %v", err)
	}
	if *reset {
		pw := randID("", 9)
		state.Config.AdminPass = shaHex(pw)
		if err := state.Save(); err != nil {
			log.Fatalf("保存失败: %v", err)
		}
		fmt.Printf("新的管理端密码（只显示这一次）：%s\n", pw)
		return
	}
	if genPass != "" {
		fmt.Printf("\n  ── 管理端密码（只显示这一次，不落盘）：%s\n", genPass)
		fmt.Printf("     忘记可执行：meoo2api --reset-password\n\n")
	}

	go state.SaveLoop()
	StartCheckinScheduler(state)
	picker := &Picker{s: state}
	html, _ := webuiFS.ReadFile("webui.html")

	handler := func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimRight(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		if path == "/" || path == "/webui.html" || path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write(html)
			return
		}
		if path == "/healthz" {
			state.mu.RLock()
			total, ck, ci := len(state.Keys), len(state.ClientKeys), len(state.Checkins)
			state.mu.RUnlock()
			usable := len(picker.Usable())
			writeJSON(w, 200, map[string]any{
				"ok": true, "upstream": apiBase, "byok": true,
				"keys": total, "usable": usable, "clientKeys": ck, "checkins": ci,
				"models": len(NativeModels), "uptime": int64(time.Since(startedAt).Seconds()),
				"alerts": state.Alerts(),
			})
			return
		}
		if state.AdminRoute(w, r, path, picker) {
			return
		}
		if path == "/v1/models" || path == "/models" || strings.HasPrefix(path, "/v1/models/") {
			c, msg := state.ResolveCaller(r)
			if msg != "" {
				oaErr(w, 401, msg, "authentication_error", "invalid_api_key")
				return
			}
			_ = c
			if !state.handleModels(w, r, path) {
				oaErr(w, 404, "未知路径", "invalid_request_error", "unknown_url")
			}
			return
		}
		if path == "/v1/chat/completions" || path == "/chat/completions" {
			state.chatHandler(w, r, picker)
			return
		}
		if path == "/v1/completions" || path == "/completions" {
			state.completionHandler(w, r, picker)
			return
		}
		if path == "/v1/responses" || path == "/responses" {
			state.responsesHandler(w, r, picker)
			return
		}
		switch path {
		case "/v1/embeddings", "/embeddings", "/v1/audio/speech", "/v1/images/generations", "/v1/moderations":
			state.mu.RLock()
			_ = state.Record
			state.mu.RUnlock()
			oaErr(w, 404, "上游不提供 "+path, "invalid_request_error", "unsupported_endpoint")
			return
		}
		oaErr(w, 404, "未知路径 "+r.Method+" "+path, "invalid_request_error", "unknown_url")
	}

	srv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", *bind, *port),
		Handler:           http.HandlerFunc(handler),
		ReadHeaderTimeout: 15 * time.Second,
	}
	state.mu.RLock()
	fmt.Printf("meoo2api(go)  http://%s:%d/\n", *bind, *port)
	fmt.Printf("  upstream %s\n", apiBase)
	fmt.Printf("  keys     %d 池内 / %d 客户端 / %d 签到\n", len(state.Keys), len(state.ClientKeys), len(state.Checkins))
	state.mu.RUnlock()
	log.Fatal(srv.ListenAndServe())
}

func (s *State) chatHandler(w http.ResponseWriter, r *http.Request, picker *Picker) {
	if r.Method != http.MethodPost {
		oaErr(w, 405, "只支持 POST", "invalid_request_error", "method_not_allowed")
		return
	}
	var in map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		oaErr(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	if msgs, ok := in["messages"].([]any); !ok || len(msgs) == 0 {
		oaErr(w, 400, "'messages' is a required property", "invalid_request_error", "missing_messages")
		return
	}
	model := s.ResolveModel(fmt.Sprint(in["model"]))
	origN := 1
	if v, ok := in["n"].(float64); ok && v > 1 {
		origN = int(v)
		if origN > 4 {
			origN = 4
		}
	}
	c, msg := s.ResolveCaller(r)
	if msg != "" {
		oaErr(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	key, label := picker.Take(c)
	payload := s.ShapeChat(in, model)
	payload["n"] = float64(origN)
	if origN > 1 {
		_ = label
	}
	s.doChat(w, r, c, key, model, payload)
}

func (s *State) completionHandler(w http.ResponseWriter, r *http.Request, picker *Picker) {
	var in map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		oaErr(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	c, msg := s.ResolveCaller(r)
	if msg != "" {
		oaErr(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	key, _ := picker.Take(c)
	s.handleLegacyCompletion(w, r, c, key, in)
}

func (s *State) responsesHandler(w http.ResponseWriter, r *http.Request, picker *Picker) {
	var in map[string]any
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		oaErr(w, 400, "请求体不是合法 JSON", "invalid_request_error", "")
		return
	}
	c, msg := s.ResolveCaller(r)
	if msg != "" {
		oaErr(w, 401, msg, "authentication_error", "invalid_api_key")
		return
	}
	key, _ := picker.Take(c)
	s.handleResponses(w, r, c, key, in)
}

func defStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
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
