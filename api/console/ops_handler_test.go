package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/health"
)

func opsAdminAuth(token string) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+token
	}
}

func TestOpsHandler_RejectsNonGet(t *testing.T) {
	h := NewOpsHandler(OpsConfig{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ops/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", resp.StatusCode)
	}
}

func TestOpsHandler_AdminAuth(t *testing.T) {
	h := NewOpsHandler(OpsConfig{
		AdminAuth: opsAdminAuth("secret"),
		Health:    func() health.Snapshot { return health.Snapshot{State: health.StateReady} },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Missing / wrong token is rejected before the data source runs.
	resp, err := http.Get(srv.URL + "/api/v1/ops/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/ops/health", nil)
	req.Header.Set("Authorization", "Bearer secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good-token status = %d, want 200", resp.StatusCode)
	}
}

func TestOpsHandler_Health(t *testing.T) {
	cases := []struct {
		state      health.State
		wantStatus string
	}{
		{health.StateReady, "ok"},
		{health.StateQuorumLost, "degraded"},
		{health.StateDraining, "degraded"},
		{health.StateStarting, "degraded"},
	}
	for _, tc := range cases {
		h := NewOpsHandler(OpsConfig{
			Health: func() health.Snapshot {
				return health.Snapshot{NodeID: "gw-1", State: tc.state}
			},
		})
		srv := httptest.NewServer(h)
		resp, err := http.Get(srv.URL + "/api/v1/ops/health")
		if err != nil {
			srv.Close()
			t.Fatal(err)
		}
		var got OpsHealthResponse
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			resp.Body.Close()
			srv.Close()
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		if got.Status != tc.wantStatus {
			t.Errorf("state %q: status = %q, want %q", tc.state, got.Status, tc.wantStatus)
		}
		if got.Node.State != tc.state {
			t.Errorf("state %q: node state = %q, want it echoed back", tc.state, got.Node.State)
		}
	}
}

func TestOpsHandler_HealthUnavailableWhenUnconfigured(t *testing.T) {
	h := NewOpsHandler(OpsConfig{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}

func TestOpsHandler_CacheStats(t *testing.T) {
	h := NewOpsHandler(OpsConfig{
		Cache: func() hot_object_cache.Stats {
			return hot_object_cache.Stats{
				Entries:    10,
				BytesUsed:  75,
				BytesLimit: 100,
				Hits:       90,
				Misses:     10,
				Evictions:  3,
			}
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/cache-stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got OpsCacheStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.HitRatio != 0.9 {
		t.Errorf("hitRatio = %v, want 0.9", got.HitRatio)
	}
	if got.Utilization != 0.75 {
		t.Errorf("utilization = %v, want 0.75", got.Utilization)
	}
	if got.Evictions != 3 {
		t.Errorf("evictions = %d, want 3", got.Evictions)
	}
}

func TestOpsHandler_CacheStatsNoLookupsAvoidsNaN(t *testing.T) {
	// Hits+Misses == 0 must not produce NaN (which is invalid JSON
	// and decodes to an error), and BytesLimit == 0 must not divide.
	h := NewOpsHandler(OpsConfig{
		Cache: func() hot_object_cache.Stats { return hot_object_cache.Stats{} },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/cache-stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got OpsCacheStatsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode (NaN would break JSON): %v", err)
	}
	if got.HitRatio != 0 || got.Utilization != 0 {
		t.Errorf("got hitRatio=%v utilization=%v, want 0/0", got.HitRatio, got.Utilization)
	}
}

func TestOpsHandler_UtilizationClampedAtOne(t *testing.T) {
	if u := utilization(150, 100); u != 1 {
		t.Errorf("utilization(150,100) = %v, want 1 (clamped)", u)
	}
}

func TestOpsHandler_WasabiBudgets(t *testing.T) {
	h := NewOpsHandler(OpsConfig{
		Wasabi: func() ([]WasabiBudget, error) {
			return []WasabiBudget{
				{TenantID: "acme", StoredBytes: 1000, EgressBytes: 800, EgressBudgetBytes: 1000, EgressRatio: 0.8, RemainingBytes: 200, Status: "warn"},
			}, nil
		},
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/wasabi-budgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got OpsWasabiBudgetsResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Budgets) != 1 || got.Budgets[0].TenantID != "acme" {
		t.Fatalf("unexpected budgets: %+v", got.Budgets)
	}
}

func TestOpsHandler_WasabiBudgetsNilBecomesEmptyArray(t *testing.T) {
	h := NewOpsHandler(OpsConfig{
		Wasabi: func() ([]WasabiBudget, error) { return nil, nil },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/wasabi-budgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// The raw body must contain `"budgets":[]`, never `"budgets":null`.
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["budgets"]) != "[]" {
		t.Errorf("budgets serialized as %s, want []", raw["budgets"])
	}
}

func TestOpsHandler_WasabiBudgetsUpstreamError(t *testing.T) {
	h := NewOpsHandler(OpsConfig{
		Wasabi: func() ([]WasabiBudget, error) { return nil, errors.New("clickhouse down") },
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/ops/wasabi-budgets")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
}

func TestOpsHandler_UnknownResource(t *testing.T) {
	h := NewOpsHandler(OpsConfig{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	for _, path := range []string{"/api/v1/ops/bogus", "/api/v1/ops/health/extra", "/api/v1/ops/"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, resp.StatusCode)
		}
	}
}
