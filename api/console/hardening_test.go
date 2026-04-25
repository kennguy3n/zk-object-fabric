package console

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// adminAuthFactory returns a predicate accepting only "Bearer tok"
// so tests can exercise the authorization boundary without exposing
// timing-sensitive crypto/subtle code to the test matrix.
func adminAuthFactory(expected string) func(r *http.Request) bool {
	return func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+expected
	}
}

func TestAdminAuth_Rejects_MissingToken(t *testing.T) {
	h := New(Config{
		Tenants:   newFakeTenantStore(sampleTenant("acme")),
		AdminAuth: adminAuthFactory("secret"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAdminAuth_Rejects_WrongToken(t *testing.T) {
	h := New(Config{
		Tenants:   newFakeTenantStore(sampleTenant("acme")),
		AdminAuth: adminAuthFactory("secret"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme", nil)
	req.Header.Set("Authorization", "Bearer nope")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestAdminAuth_Accepts_CorrectToken(t *testing.T) {
	h := New(Config{
		Tenants:   newFakeTenantStore(sampleTenant("acme")),
		AdminAuth: adminAuthFactory("secret"),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// --- new resource routes ---------------------------------------

func TestListBuckets_EmptyWhenNoStore(t *testing.T) {
	h := New(Config{
		Tenants: newFakeTenantStore(sampleTenant("acme")),
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/buckets", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var out []BucketDescriptor
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if out == nil {
		t.Fatalf("buckets should be an empty array, got nil")
	}
}

func TestCreateBucket_RoundTrip(t *testing.T) {
	store := NewMemoryBucketStore()
	h := New(Config{
		Tenants: newFakeTenantStore(sampleTenant("acme")),
		Buckets: store,
	})
	body := strings.NewReader(`{"name":"alpha","placementPolicyRef":"p1"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tenants/acme/buckets", body)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	// List returns the created bucket.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/tenants/acme/buckets", nil)
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "\"alpha\"") {
		t.Fatalf("list body did not contain created bucket; body = %s", rec.Body.String())
	}

	// Delete removes it.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/tenants/acme/buckets/alpha", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rec.Code)
	}
}

// listKeysTenantStore extends the fakeTenantStore surface with the
// APIKeyLister methods so we can verify the GET/DELETE paths.
type listKeysTenantStore struct {
	*fakeTenantStore
	keys map[string][]APIKeyDescriptor
}

func (s *listKeysTenantStore) ListAPIKeys(tenantID string) ([]APIKeyDescriptor, error) {
	if ks, ok := s.keys[tenantID]; ok {
		return ks, nil
	}
	return nil, nil
}

func (s *listKeysTenantStore) DeleteAPIKey(tenantID, accessKey string) error {
	if s.keys == nil {
		return errors.New("no keys")
	}
	ks := s.keys[tenantID]
	filtered := ks[:0]
	for _, k := range ks {
		if k.AccessKey != accessKey {
			filtered = append(filtered, k)
		}
	}
	s.keys[tenantID] = filtered
	return nil
}

func TestListKeys_AndRevoke(t *testing.T) {
	base := newFakeTenantStore(sampleTenant("acme"))
	store := &listKeysTenantStore{
		fakeTenantStore: base,
		keys:            map[string][]APIKeyDescriptor{"acme": {{AccessKey: "AKIA1"}}},
	}
	h := New(Config{Tenants: store})

	// list
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/keys", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "AKIA1") {
		t.Fatalf("list body missing key; body = %s", rec.Body.String())
	}

	// delete
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/tenants/acme/keys/AKIA1", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if len(store.keys["acme"]) != 0 {
		t.Fatalf("expected keys to be empty after delete, got %v", store.keys["acme"])
	}
}

func TestListDedicatedCells_EmptyWhenNoStore(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/dedicated-cells", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Fatalf("body = %q, want []", rec.Body.String())
	}
}

// --- signup billing emission ------------------------------------

type recordingBillingSink struct {
	events []billing.UsageEvent
}

func (s *recordingBillingSink) Emit(event billing.UsageEvent) {
	s.events = append(s.events, event)
}

func TestSignup_EmitsTenantCreatedBillingEvent(t *testing.T) {
	sink := &recordingBillingSink{}
	authStore := NewMemoryAuthStore()
	tokens := NewMemoryTokenStore()
	h := NewAuthHandler(AuthConfig{
		Tenants:     newFakeTenantStore(),
		Auth:        authStore,
		Tokens:      tokens,
		BillingSink: sink,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body := strings.NewReader(`{"email":"a@b.com","password":"SuperSecretPass123","tenantName":"Acme"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", body)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if len(sink.events) != 1 {
		t.Fatalf("sink events = %d, want 1", len(sink.events))
	}
	evt := sink.events[0]
	if evt.Dimension != billing.TenantCreated {
		t.Fatalf("event dimension = %q, want %q", evt.Dimension, billing.TenantCreated)
	}
	if evt.Delta != 1 {
		t.Fatalf("event delta = %d, want 1", evt.Delta)
	}
	if evt.TenantID == "" {
		t.Fatalf("event TenantID is empty")
	}
}

// --- BillingProvider EnsureCustomer wiring ----------------------

// recordingBillingProvider captures EnsureCustomer calls made by the
// signup flow so tests can assert the per-signup CustomerRequest
// shape without standing up a real provider.
type recordingBillingProvider struct {
	mu       sync.Mutex
	requests []billing.CustomerRequest
	err      error
}

func (p *recordingBillingProvider) Name() string { return "recording" }

func (p *recordingBillingProvider) EnsureCustomer(ctx context.Context, req billing.CustomerRequest) (billing.CustomerHandle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if p.err != nil {
		return billing.CustomerHandle{}, p.err
	}
	return billing.CustomerHandle{Provider: "recording", ProviderRef: "cus_" + req.TenantID}, nil
}

func (p *recordingBillingProvider) EnsureSubscription(ctx context.Context, req billing.SubscriptionRequest) (billing.SubscriptionHandle, error) {
	return billing.SubscriptionHandle{}, nil
}
func (p *recordingBillingProvider) ReportUsage(ctx context.Context, events []billing.UsageEvent) error {
	return nil
}
func (p *recordingBillingProvider) IssueInvoice(ctx context.Context, req billing.InvoiceRequest) (billing.InvoiceHandle, error) {
	return billing.InvoiceHandle{}, nil
}
func (p *recordingBillingProvider) CancelSubscription(ctx context.Context, subscriptionID string) error {
	return nil
}

func TestSignup_CallsBillingProviderEnsureCustomer(t *testing.T) {
	provider := &recordingBillingProvider{}
	h := NewAuthHandler(AuthConfig{
		Tenants:         newFakeTenantStore(),
		Auth:            NewMemoryAuthStore(),
		Tokens:          NewMemoryTokenStore(),
		BillingProvider: provider,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body := strings.NewReader(`{"email":"customer@example.com","password":"SuperSecretPass123","tenantName":"Acme Inc"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", body)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %d, want 1", len(provider.requests))
	}
	got := provider.requests[0]
	if got.Email != "customer@example.com" {
		t.Errorf("CustomerRequest.Email = %q, want %q", got.Email, "customer@example.com")
	}
	if got.Name != "Acme Inc" {
		t.Errorf("CustomerRequest.Name = %q, want %q", got.Name, "Acme Inc")
	}
	if got.TenantID == "" {
		t.Errorf("CustomerRequest.TenantID is empty")
	}
	if got.Metadata["signup_path"] != "b2c_self_service" {
		t.Errorf("CustomerRequest.Metadata[signup_path] = %q, want %q",
			got.Metadata["signup_path"], "b2c_self_service")
	}
}

// TestSignup_EnsureCustomerFailureDoesNotRollBackTenant verifies a
// failed EnsureCustomer is logged but the signup still returns 201
// — the tenant + API key + token were already minted and the
// provider can be reconciled later by a sweep job.
func TestSignup_EnsureCustomerFailureDoesNotRollBackTenant(t *testing.T) {
	provider := &recordingBillingProvider{err: errors.New("upstream 500")}
	tenants := newFakeTenantStore()
	h := NewAuthHandler(AuthConfig{
		Tenants:         tenants,
		Auth:            NewMemoryAuthStore(),
		Tokens:          NewMemoryTokenStore(),
		BillingProvider: provider,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body := strings.NewReader(`{"email":"a@b.com","password":"SuperSecretPass123","tenantName":"Acme"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", body)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	tenants.mu.Lock()
	count := len(tenants.tenants)
	tenants.mu.Unlock()
	if count != 1 {
		t.Fatalf("tenants in store = %d, want 1 (signup must not roll back on EnsureCustomer failure)", count)
	}
}

// --- tenant-scoped SSE alias ------------------------------------

// fakeTokenLookup is a TokenStore stub that resolves a single
// (token → tenantID) pair for the SSE alias test.
type fakeTokenLookup struct {
	token, tenantID string
}

func (s *fakeTokenLookup) IssueToken(tenantID string) (string, error) {
	if s.tenantID == tenantID {
		return s.token, nil
	}
	return "", errors.New("issue not supported")
}

func (s *fakeTokenLookup) ResolveToken(token string) (string, bool) {
	if token == s.token {
		return s.tenantID, true
	}
	return "", false
}

// TestUsageStream_TenantScopedAlias verifies the console-mux alias
// /api/tenants/{id}/usage/stream resolves through the same handler
// as the legacy /api/v1/usage/stream/{id} path.
func TestUsageStream_TenantScopedAlias(t *testing.T) {
	tokens := &fakeTokenLookup{token: "tok123", tenantID: "acme"}
	h := New(Config{
		Tenants:             newFakeTenantStore(sampleTenant("acme")),
		Usage:               &fakeUsage{result: map[string]uint64{"egress_bytes": 42}},
		Tokens:              tokens,
		UsageStreamInterval: time.Hour, // ensure only the initial frame fires before we close
	})
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/usage/stream?token=tok123", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 250*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "egress_bytes") {
		t.Errorf("body missing initial frame counters; body = %s", rec.Body.String())
	}
}

// TestUsageStream_TenantScopedAlias_NotConfigured verifies the
// alias degrades to 503 when Usage is not configured, matching the
// legacy /api/v1 path.
func TestUsageStream_TenantScopedAlias_NotConfigured(t *testing.T) {
	h := New(Config{Tenants: newFakeTenantStore(sampleTenant("acme"))})
	mux := http.NewServeMux()
	h.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tenants/acme/usage/stream", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

// --- CAPTCHA rejection ------------------------------------------

func TestSignup_RejectsWhenCAPTCHAHookFails(t *testing.T) {
	authStore := NewMemoryAuthStore()
	tokens := NewMemoryTokenStore()
	h := NewAuthHandler(AuthConfig{
		Tenants: newFakeTenantStore(),
		Auth:    authStore,
		Tokens:  tokens,
		Hooks: AuthHooks{
			VerifyCAPTCHA: func(token string) error {
				return errors.New("captcha rejected")
			},
		},
	})
	mux := http.NewServeMux()
	h.Register(mux)

	body := strings.NewReader(`{"email":"a@b.com","password":"SuperSecretPass123","tenantName":"Acme","captchaToken":"bogus"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/signup", body)
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}
