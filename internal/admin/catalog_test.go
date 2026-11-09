package admin

import (
	"net/http"
	"testing"

	"meoo2api/internal/catalog"
)

func TestStateUsesLiveCatalogAndSyncMetadata(t *testing.T) {
	h, token := fixture(t)
	seedAccount(h)
	h.Acc.Up.HTTP.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected model request: %s", r.URL)
		}
		return response(200, `{"models":["new-upstream-model"],"defaultModel":"new-upstream-model"}`), nil
	})
	h.Catalog = catalog.New(h.St, h.Acc.Up)
	w := request(h, token, "GET", "/admin/api/state", "")
	expect(t, w, 200)
	d := object(t, w)
	models := d["models"].([]any)
	if len(models) != 1 || models[0] != "new-upstream-model" {
		t.Fatal("admin state still uses static models", models)
	}
	meta := d["modelSync"].(map[string]any)
	if meta["source"] != "upstream" || meta["stale"] != false || meta["defaultModel"] != "new-upstream-model" || meta["updatedAt"].(float64) <= 0 {
		t.Fatal("sync state missing", meta)
	}
}
