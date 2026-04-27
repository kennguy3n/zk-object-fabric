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
