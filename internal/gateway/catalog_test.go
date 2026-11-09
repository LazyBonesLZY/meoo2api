package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"meoo2api/internal/catalog"
)

func TestUpstreamCatalogIsAuthoritativeAndSharedWithModelDetail(t *testing.T) {
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || r.Method != "GET" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []string{"fresh-model", "another-model"}, "defaultModel": "fresh-model"})
	})
	h.Catalog = catalog.New(h.St, h.Up)
	h.St.Data.Config.Aliases = map[string]string{"fresh": "fresh-model", "old": "qwen3.8-flash"}
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Authorization", "Bearer x")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := get("/v1/models")
	if w.Code != 200 || w.Header().Get("X-Meoo-Models-Source") != "upstream" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	d := decodeBody(t, w)
	entries := d["data"].([]any)
	ids := map[string]bool{}
	for _, entry := range entries {
		ids[entry.(map[string]any)["id"].(string)] = true
	}
	if len(ids) != 3 || !ids["fresh-model"] || !ids["fresh"] || ids["old"] || ids["qwen3.8-flash"] {
		t.Fatal("upstream directory mixed with obsolete builtins or aliases", ids)
	}
	if w = get("/v1/models/fresh-model"); w.Code != 200 || decodeBody(t, w)["id"] != "fresh-model" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = get("/v1/models/fresh"); w.Code != 200 || decodeBody(t, w)["owned_by"] != "meoo-alias" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = get("/v1/models/qwen3.8-flash"); w.Code != 404 {
		t.Fatal("deleted upstream model remains in detail", w.Code)
	}
	h.St.Data.Config.HiddenModels = []string{"fresh-model"}
	if w = get("/v1/models/fresh"); w.Code != 404 {
		t.Fatal("alias bypasses hidden target")
	}
}

func TestBYOKCatalogDoesNotPollutePoolDirectory(t *testing.T) {
	h, _ := fixture(t, func(w http.ResponseWriter, r *http.Request) {
		name := "pool-model"
		if r.Header.Get("Authorization") == "Bearer meoo_project_byok_fixture" {
			name = "private-model"
		}
		json.NewEncoder(w).Encode(map[string]any{"models": []string{name}})
	})
	h.Catalog = catalog.New(h.St, h.Up)
	for _, tc := range []struct{ key, want string }{{"x", "pool-model"}, {"meoo_project_byok_fixture", "private-model"}, {"x", "pool-model"}} {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+tc.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		d := decodeBody(t, w)["data"].([]any)
		if len(d) != 1 || d[0].(map[string]any)["id"] != tc.want {
			t.Fatal("model scopes mixed", d)
		}
	}
}
