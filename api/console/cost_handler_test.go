package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

type fakeReporter struct {
	bd    billing.CostBreakdown
	err   error
	gotID string
	gotMo string
	calls int
}

func (f *fakeReporter) GetCostBreakdown(_ context.Context, tenantID, month string) (billing.CostBreakdown, error) {
	f.calls++
	f.gotID = tenantID
	f.gotMo = month
	if f.err != nil {
		return billing.CostBreakdown{}, f.err
	}
	bd := f.bd
	bd.TenantID = tenantID
	bd.Month = month
	return bd, nil
}

func TestCostHandler_ReturnsBreakdown(t *testing.T) {
	rep := &fakeReporter{bd: billing.CostBreakdown{WasabiStorageUSD: 6.8, TotalUSD: 5.2, DedupSavingsUSD: 1.6}}
	h := &CostHandler{Reporter: rep}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown?month=2026-06")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got billing.CostBreakdown
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.TenantID != "t-1" || got.Month != "2026-06" || got.WasabiStorageUSD != 6.8 {
		t.Errorf("unexpected breakdown: %+v", got)
	}
	if rep.gotID != "t-1" || rep.gotMo != "2026-06" {
		t.Errorf("reporter called with (%q,%q)", rep.gotID, rep.gotMo)
	}
}

func TestCostHandler_DefaultsMonthWhenAbsent(t *testing.T) {
	rep := &fakeReporter{}
	h := &CostHandler{Reporter: rep}
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Empty month is forwarded to the reporter, which resolves it.
	if rep.gotMo != "" {
		t.Errorf("expected empty month forwarded, got %q", rep.gotMo)
	}
}

func TestCostHandler_RejectsInvalidMonth(t *testing.T) {
	h := &CostHandler{Reporter: &fakeReporter{}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown?month=June-2026")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCostHandler_AdminAuthGate(t *testing.T) {
	rep := &fakeReporter{}
	h := &CostHandler{Reporter: rep, AdminAuth: func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer ok"
	}}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// No token -> 401, reporter never consulted.
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if rep.calls != 0 {
		t.Errorf("reporter consulted despite failed auth (%d calls)", rep.calls)
	}

	// Correct token -> 200.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/tenants/t-1/cost-breakdown", nil)
	req.Header.Set("Authorization", "Bearer ok")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestCostHandler_NilReporterIsServiceUnavailable(t *testing.T) {
	h := &CostHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestCostHandler_RejectsNonGet(t *testing.T) {
	h := &CostHandler{Reporter: &fakeReporter{}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tenants/t-1/cost-breakdown", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestCostHandler_NotFoundForBadPath(t *testing.T) {
	h := &CostHandler{Reporter: &fakeReporter{}}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/something-else")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestCostHandler_RegisterRouting exercises the Go 1.22 pattern
// registration path (PathValue extraction) rather than ServeHTTP's
// manual parse.
func TestCostHandler_RegisterRouting(t *testing.T) {
	rep := &fakeReporter{}
	mux := http.NewServeMux()
	(&CostHandler{Reporter: rep}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-42/cost-breakdown?month=2026-01")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if rep.gotID != "t-42" || rep.gotMo != "2026-01" {
		t.Errorf("routing extracted (%q,%q), want (t-42,2026-01)", rep.gotID, rep.gotMo)
	}
}

// TestConsoleRegister_MountsCostRouteWhenReporterSet verifies the
// console Handler.Register wiring: the cost route is mounted only
// when Config.CostReporter is set, and it inherits the console's
// AdminAuth gate.
func TestConsoleRegister_MountsCostRouteWhenReporterSet(t *testing.T) {
	rep := &fakeReporter{}
	admitted := false
	mux := http.NewServeMux()
	New(Config{
		CostReporter: rep,
		AdminAuth:    func(*http.Request) bool { return admitted },
	}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// AdminAuth denies → 401 and the reporter is never consulted.
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-9/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("denied status = %d, want 401", resp.StatusCode)
	}
	if rep.calls != 0 {
		t.Fatalf("reporter called %d times despite admin denial", rep.calls)
	}

	// AdminAuth admits → route serves the breakdown.
	admitted = true
	resp2, err := http.Get(srv.URL + "/api/v1/tenants/t-9/cost-breakdown?month=2026-06")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("admitted status = %d, want 200", resp2.StatusCode)
	}
	if rep.gotID != "t-9" || rep.gotMo != "2026-06" {
		t.Errorf("reporter called with (%q,%q), want (t-9,2026-06)", rep.gotID, rep.gotMo)
	}
}

// TestConsoleServeHTTP_RoutesCostPathWhenDedupEnabled covers the
// ServeHTTP (non-ServeMux) entry point: when both DedupPolicies and
// CostReporter are configured, a cost-breakdown request must reach
// the cost handler rather than falling into dispatchDedup (which
// would 400 the 2-segment cost path).
func TestConsoleServeHTTP_RoutesCostPathWhenDedupEnabled(t *testing.T) {
	rep := &fakeReporter{}
	h := New(Config{
		CostReporter:  rep,
		DedupPolicies: NewMemoryDedupPolicyStore(),
	})
	// Register populates h.costHandler; the handler itself (not the
	// mux) is the surface under test.
	h.Register(http.NewServeMux())
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-7/cost-breakdown?month=2026-06")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (cost path must not fall into dispatchDedup)", resp.StatusCode)
	}
	if rep.gotID != "t-7" || rep.gotMo != "2026-06" {
		t.Errorf("reporter called with (%q,%q), want (t-7,2026-06)", rep.gotID, rep.gotMo)
	}
}

// TestConsoleRegister_CostRouteTenantExistence verifies the
// tenant-existence gate the console wires from Config.Tenants: an
// unknown tenant 404s without consulting the reporter, a known
// tenant serves.
func TestConsoleRegister_CostRouteTenantExistence(t *testing.T) {
	rep := &fakeReporter{}
	tenants := newFakeTenantStore(sampleTenant("t-known"))
	mux := http.NewServeMux()
	New(Config{CostReporter: rep, Tenants: tenants}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-missing/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown tenant status = %d, want 404", resp.StatusCode)
	}
	if rep.calls != 0 {
		t.Fatalf("reporter called %d times for unknown tenant", rep.calls)
	}

	resp2, err := http.Get(srv.URL + "/api/v1/tenants/t-known/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("known tenant status = %d, want 200", resp2.StatusCode)
	}
}

// TestConsoleRegister_SkipsCostRouteWhenReporterNil verifies the
// route is absent (404, not 503) when no reporter is configured, so a
// deployment without a cost model does not advertise the endpoint.
func TestConsoleRegister_SkipsCostRouteWhenReporterNil(t *testing.T) {
	mux := http.NewServeMux()
	New(Config{}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/tenants/t-1/cost-breakdown")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (route should be unmounted)", resp.StatusCode)
	}
}
