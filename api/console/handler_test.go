package console

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// fakeTenantStore is a TenantStore backed by a simple map so tests
// can pre-seed tenants and observe AddAPIKey side effects.
type fakeTenantStore struct {
	mu       sync.Mutex
	tenants  map[string]tenant.Tenant
	bindings []binding
}

type binding struct {
	tenantID, accessKey, secretKey string
}

func newFakeTenantStore(ts ...tenant.Tenant) *fakeTenantStore {
	s := &fakeTenantStore{tenants: map[string]tenant.Tenant{}}
	for _, t := range ts {
		s.tenants[t.ID] = t
	}
	return s
}

func (s *fakeTenantStore) LookupTenant(id string) (tenant.Tenant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tenants[id]
	return t, ok
}

func (s *fakeTenantStore) AddAPIKey(tenantID, accessKey, secretKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if b.accessKey == accessKey {
			return fmt.Errorf("duplicate access key %q", accessKey)
		}
	}
	s.bindings = append(s.bindings, binding{tenantID, accessKey, secretKey})
	return nil
}

func (s *fakeTenantStore) CreateTenant(t tenant.Tenant) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tenants[t.ID]; ok {
		return fmt.Errorf("tenant %q already exists", t.ID)
	}
	s.tenants[t.ID] = t
	return nil
}

// fakeUsage is a UsageQuery that returns pre-canned counter maps.
type fakeUsage struct {
	result map[string]uint64
	err    error
	calls  []usageCall
}

type usageCall struct {
	tenantID   string
	start, end time.Time
}

func (u *fakeUsage) TenantUsage(ctx context.Context, tenantID string, start, end time.Time) (map[string]uint64, error) {
	u.calls = append(u.calls, usageCall{tenantID, start, end})
	if u.err != nil {
		return nil, u.err
	}
	return u.result, nil
}

// fakePlacementStore is an in-memory PlacementStore suitable for
// tests.
type fakePlacementStore struct {
	mu       sync.Mutex
	policies map[string]placement_policy.Policy
}

func newFakePlacementStore() *fakePlacementStore {
	return &fakePlacementStore{policies: map[string]placement_policy.Policy{}}
}

func (s *fakePlacementStore) GetPlacement(ctx context.Context, tenantID string) (placement_policy.Policy, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.policies[tenantID]
	return p, ok, nil
}

func (s *fakePlacementStore) PutPlacement(ctx context.Context, tenantID string, policy placement_policy.Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[tenantID] = policy
	return nil
}

func sampleTenant(id string) tenant.Tenant {
	return tenant.Tenant{
		ID:               id,
		Name:             id,
		ContractType:     tenant.ContractB2CPooled,
		LicenseTier:      tenant.LicenseStandard,
		Keys:             tenant.Keys{RootKeyRef: "cmk://" + id, DEKPolicy: tenant.DEKPerObject},
		PlacementDefault: tenant.PlacementDefault{PolicyRef: "p"},
		Billing:          tenant.Billing{Currency: "USD"},
	}
}

func TestGetTenant(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var got tenant.Tenant
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "acme" {
		t.Fatalf("ID = %q, want acme", got.ID)
	}
}

func TestGetTenant_NotFound(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/missing", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetUsage_DefaultWindow(t *testing.T) {
	now := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	usage := &fakeUsage{result: map[string]uint64{"egress_bytes": 1024}}
	h := New(Config{
		Tenants: newFakeTenantStore(sampleTenant("acme")),
		Usage:   usage,
		Now:     func() time.Time { return now },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/usage", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var got UsageResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TenantID != "acme" {
		t.Fatalf("tenant = %q, want acme", got.TenantID)
	}
	if got.Counters["egress_bytes"] != 1024 {
		t.Fatalf("counters = %v, want egress_bytes=1024", got.Counters)
	}
	if len(usage.calls) != 1 {
		t.Fatalf("usage.calls = %d, want 1", len(usage.calls))
	}
	call := usage.calls[0]
	if !call.end.Equal(now) {
		t.Fatalf("end = %v, want %v", call.end, now)
	}
	wantStart := now.Add(-30 * 24 * time.Hour)
	if !call.start.Equal(wantStart) {
		t.Fatalf("start = %v, want %v", call.start, wantStart)
	}
}

func TestGetUsage_ExplicitWindow(t *testing.T) {
	usage := &fakeUsage{result: map[string]uint64{}}
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme")), Usage: usage})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/usage?start=2026-04-01T00:00:00Z&end=2026-04-22T00:00:00Z", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	if len(usage.calls) != 1 {
		t.Fatalf("usage.calls = %d, want 1", len(usage.calls))
	}
	if !usage.calls[0].start.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected start: %v", usage.calls[0].start)
	}
}

func TestGetUsage_InvalidRange(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme")), Usage: &fakeUsage{}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/usage?start=2026-05-01T00:00:00Z&end=2026-04-01T00:00:00Z", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCreateKey(t *testing.T) {
	store := newFakeTenantStore(sampleTenant("acme"))
	h := New(Config{
		Tenants: store,
		GenerateKey: func() (string, string, error) {
			return "AKIADETERMINISTIC", "SKDETERMINISTIC", nil
		},
		Now: func() time.Time { return time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC) },
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tenants/acme/keys", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var got CreateKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.AccessKey != "AKIADETERMINISTIC" || got.SecretKey != "SKDETERMINISTIC" {
		t.Fatalf("unexpected keys: %+v", got)
	}
	if len(store.bindings) != 1 {
		t.Fatalf("bindings = %d, want 1", len(store.bindings))
	}
	if store.bindings[0].tenantID != "acme" {
		t.Fatalf("binding tenant = %q, want acme", store.bindings[0].tenantID)
	}
}

func TestCreateKey_UnknownTenant(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tenants/missing/keys", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestCreateKey_GeneratorError(t *testing.T) {
	h := New(Config{
		Tenants:     newFakeTenantStore(sampleTenant("acme")),
		GenerateKey: func() (string, string, error) { return "", "", errors.New("rand failed") },
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tenants/acme/keys", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestGetPlacement_EmptyShell(t *testing.T) {
	h := New(Config{Placements: newFakePlacementStore()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/placement", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	var got placement_policy.Policy
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", got.Tenant)
	}
}

func TestPutPlacement_RoundTrip(t *testing.T) {
	store := newFakePlacementStore()
	h := New(Config{Placements: store})

	pol := placement_policy.Policy{
		Tenant: "acme",
		Spec: placement_policy.PolicySpec{
			Encryption: placement_policy.EncryptionSpec{Mode: "managed", KMS: "cmk://acme"},
			Placement:  placement_policy.PlacementSpec{Provider: []string{"wasabi"}, Country: []string{"SG"}},
		},
	}
	body, _ := json.Marshal(pol)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/tenants/acme/placement", bytes.NewReader(body))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}

	stored, ok, _ := store.GetPlacement(context.Background(), "acme")
	if !ok {
		t.Fatal("policy not persisted")
	}
	if len(stored.Spec.Placement.Provider) != 1 || stored.Spec.Placement.Provider[0] != "wasabi" {
		t.Fatalf("provider = %v, want [wasabi]", stored.Spec.Placement.Provider)
	}

	// GET should now return the persisted policy.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/tenants/acme/placement", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body = %s", rec.Code, rec.Body.String())
	}
}

func TestPutPlacement_PathBindingWins(t *testing.T) {
	store := newFakePlacementStore()
	h := New(Config{Placements: store})
	// Body claims tenant=attacker but path says acme; the path
	// must win so a confused caller cannot overwrite the wrong
	// tenant's policy.
	body := `{"tenant":"attacker","policy":{"encryption":{"mode":"managed"},"placement":{"provider":["wasabi"]}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/tenants/acme/placement", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	stored, _, _ := store.GetPlacement(context.Background(), "acme")
	if stored.Tenant != "acme" {
		t.Fatalf("stored tenant = %q, want acme", stored.Tenant)
	}
	// The attacker's tenant ID should not have a policy.
	if _, ok, _ := store.GetPlacement(context.Background(), "attacker"); ok {
		t.Fatal("attacker tenant should not have a policy")
	}
}

func TestPutPlacement_Invalid(t *testing.T) {
	h := New(Config{Placements: newFakePlacementStore()})
	// Missing placement.provider — Validate should reject.
	body := `{"policy":{"encryption":{"mode":"managed"},"placement":{}}}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/tenants/acme/placement", strings.NewReader(body))
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestDispatch_MethodNotAllowed(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})
	// DELETE on /api/tenants/{id} is not supported.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/tenants/acme", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

func TestDispatch_InvalidPath(t *testing.T) {
	h := New(Config{})
	cases := []string{
		"/api/tenants/",
		"/api/tenants",
		"/api/tenants/acme/keys/extra",
	}
	for _, path := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("path %q returned 200", path)
		}
	}
}

func TestRegister_AttachesRoutes(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
}

func TestDefaultKeyGenerator_ProducesDistinctPairs(t *testing.T) {
	a1, s1, err := defaultKeyGenerator()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	a2, s2, err := defaultKeyGenerator()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if a1 == a2 || s1 == s2 {
		t.Fatalf("expected distinct pairs, got (%s,%s) and (%s,%s)", a1, s1, a2, s2)
	}
	if len(a1) != 20 || len(s1) != 40 {
		t.Fatalf("unexpected key lengths: access=%d secret=%d", len(a1), len(s1))
	}
}
