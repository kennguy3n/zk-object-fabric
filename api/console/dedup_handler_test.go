package console

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
)

// fakeDedicatedCellStore lets dedup_handler tests assert that the
// "object+block" guardrail consults the cell store. Empty list ⇒ no
// dedicated cell; populated list ⇒ at least one bound cell.
type fakeDedicatedCellStore struct {
	cells []DedicatedCellDescriptor
}

func (s *fakeDedicatedCellStore) ListDedicatedCells(_ context.Context, _ string) ([]DedicatedCellDescriptor, error) {
	return s.cells, nil
}

func newDedupTestHandler(t *testing.T, opts ...func(*Config)) *Handler {
	t.Helper()
	tenants := newFakeTenantStore(sampleTenant("acme"))
	placements := &fakePlacementStore{policies: map[string]placement_policy.Policy{}}
	cfg := Config{
		Tenants:       tenants,
		Placements:    placements,
		DedupPolicies: NewMemoryDedupPolicyStore(),
		Cells:         &fakeDedicatedCellStore{},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return New(cfg)
}

func TestDedupPolicy_GetMissingReturnsDisabled(t *testing.T) {
	h := newDedupTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/acme/buckets/photos/dedup-policy", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["enabled"] != false {
		t.Fatalf("enabled = %v want false", got["enabled"])
	}
}

func TestDedupPolicy_PostObjectLevelEnables(t *testing.T) {
	h := newDedupTestHandler(t)
	body := bytes.NewBufferString(`{"enabled":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/buckets/photos/dedup-policy", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200, body = %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["enabled"] != true {
		t.Fatalf("enabled = %v want true", got["enabled"])
	}
	if got["scope"] != "intra_tenant" {
		t.Fatalf("scope = %v want intra_tenant", got["scope"])
	}
	if got["level"] != "object" {
		t.Fatalf("level = %v want object (default)", got["level"])
	}

	// GET reflects the write.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tenants/acme/buckets/photos/dedup-policy", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d want 200", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["enabled"] != true {
		t.Fatalf("after PUT, enabled = %v want true", got["enabled"])
	}
}

func TestDedupPolicy_RejectsCrossTenantScope(t *testing.T) {
	h := newDedupTestHandler(t)
	body := bytes.NewBufferString(`{"enabled":true,"scope":"cross_tenant"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/buckets/photos/dedup-policy", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDedupPolicy_ObjectBlockRequiresCephAndDedicatedCell(t *testing.T) {
	// No placement, no cells -> reject.
	h := newDedupTestHandler(t)
	body := bytes.NewBufferString(`{"enabled":true,"level":"object+block"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/buckets/photos/dedup-policy", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 (no placement)", rec.Code)
	}

	// With a Ceph placement AND a dedicated cell -> accept.
	tenants := newFakeTenantStore(sampleTenant("acme"))
	placements := &fakePlacementStore{policies: map[string]placement_policy.Policy{
		"acme": {
			Spec: placement_policy.PolicySpec{
				Placement: placement_policy.PlacementSpec{Provider: []string{"ceph_rgw"}},
			},
		},
	}}
	cells := &fakeDedicatedCellStore{cells: []DedicatedCellDescriptor{{ID: "cell-1"}}}
	h2 := New(Config{
		Tenants:       tenants,
		Placements:    placements,
		DedupPolicies: NewMemoryDedupPolicyStore(),
		Cells:         cells,
	})
	body = bytes.NewBufferString(`{"enabled":true,"level":"object+block"}`)
	req = httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/buckets/photos/dedup-policy", body)
	rec = httptest.NewRecorder()
	h2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200, body = %s", rec.Code, rec.Body.String())
	}
}

func TestDedupPolicy_DeleteRemovesPolicy(t *testing.T) {
	h := newDedupTestHandler(t)
	// Enable first.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tenants/acme/buckets/photos/dedup-policy", bytes.NewBufferString(`{"enabled":true}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable status = %d", rec.Code)
	}
	// Then delete.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/tenants/acme/buckets/photos/dedup-policy", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d want 204", rec.Code)
	}
	// GET now returns disabled.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/tenants/acme/buckets/photos/dedup-policy", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["enabled"] != false {
		t.Fatalf("after delete, enabled = %v want false", got["enabled"])
	}
}

func TestDedupPolicy_RejectsMalformedPath(t *testing.T) {
	h := newDedupTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tenants/acme/buckets/photos", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400", rec.Code)
	}
}

func TestParseDedupPath(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		tenant   string
		bucket   string
	}{
		{"/api/v1/tenants/t1/buckets/b1/dedup-policy", true, "t1", "b1"},
		{"/api/v1/tenants/t1/buckets/b1/dedup-policy/", true, "t1", "b1"},
		{"/api/v1/tenants/t1/buckets/b1", false, "", ""},
		{"/api/v1/tenants/t1/buckets/b1/something-else", false, "", ""},
		{"/api/v1/tenants//buckets/b1/dedup-policy", false, "", ""},
		{"/wrong/prefix/t1/buckets/b1/dedup-policy", false, "", ""},
	}
	for _, tc := range cases {
		gotT, gotB, gotOK := parseDedupPath(tc.in)
		if gotOK != tc.ok || gotT != tc.tenant || gotB != tc.bucket {
			t.Errorf("parseDedupPath(%q) = (%q,%q,%v) want (%q,%q,%v)", tc.in, gotT, gotB, gotOK, tc.tenant, tc.bucket, tc.ok)
		}
	}
}
