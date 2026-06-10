package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

func TestTierHandler_ListsAllDefaults(t *testing.T) {
	srv := httptest.NewServer(&TierHandler{})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tiers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got []tenant.TierConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(tenant.DefaultTierConfigs()) {
		t.Errorf("len=%d, want %d", len(got), len(tenant.DefaultTierConfigs()))
	}
}

func TestTierHandler_RejectsNonGet(t *testing.T) {
	srv := httptest.NewServer(&TierHandler{})
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tiers", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status=%d, want 405", resp.StatusCode)
	}
}

// TestTierRoute_ServeHTTPMatchesRegister guards the symmetry between
// Handler.Register (mux mount, exact "/api/v1/tiers" pattern) and
// Handler.ServeHTTP (direct mount). Both must serve the catalogue on
// the exact path, and — critically — neither may serve it for the
// "/api/v1/tiers/" trailing-slash variant: Go's mux treats the
// slash-free pattern as an exact match, so a ServeHTTP intercept that
// trimmed the trailing slash would over-match and disagree with the
// mux mount.
func TestTierRoute_ServeHTTPMatchesRegister(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})
	mux := http.NewServeMux()
	h.Register(mux)

	serve := func(handler http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	// Exact path: both surfaces serve the catalogue identically.
	muxExact := serve(mux, "/api/v1/tiers")
	directExact := serve(h, "/api/v1/tiers")
	if muxExact.Code != http.StatusOK || directExact.Code != http.StatusOK {
		t.Fatalf("exact path: mux=%d direct=%d, want both 200", muxExact.Code, directExact.Code)
	}
	if muxExact.Body.String() != directExact.Body.String() {
		t.Errorf("exact path bodies differ:\n mux=%s\n direct=%s", muxExact.Body.String(), directExact.Body.String())
	}

	// Trailing-slash variant: neither surface serves the catalogue.
	if got := serve(mux, "/api/v1/tiers/").Code; got == http.StatusOK {
		t.Errorf("mux served /api/v1/tiers/ as 200; want a non-200 (exact pattern should not match)")
	}
	if got := serve(h, "/api/v1/tiers/").Code; got == http.StatusOK {
		t.Errorf("ServeHTTP served /api/v1/tiers/ as 200; want a non-200 to mirror the mux mount")
	}
}
