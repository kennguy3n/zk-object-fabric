package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
