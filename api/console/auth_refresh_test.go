package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newRefreshTestHandler wires an AuthHandler with in-memory stores and
// a refresh-token store, returning the mux and the refresh store so a
// test can both drive the HTTP surface and inspect server-side state.
func newRefreshTestHandler(t *testing.T, refresh RefreshTokenStore) *http.ServeMux {
	t.Helper()
	h := NewAuthHandler(AuthConfig{
		Tenants:       newFakeTenantStore(),
		Auth:          NewMemoryAuthStore(),
		Tokens:        NewMemoryTokenStore(),
		RefreshTokens: refresh,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

func postJSON(t *testing.T, mux *http.ServeMux, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	return rec
}

// signupForRefresh runs a signup and returns the issued refresh token.
func signupForRefresh(t *testing.T, mux *http.ServeMux, email string) AuthResponse {
	t.Helper()
	rec := postJSON(t, mux, authPathSignup,
		`{"email":"`+email+`","password":"SuperSecretPass123","tenantName":"Acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	var resp AuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode signup response: %v", err)
	}
	return resp
}

func TestSignupReturnsRefreshTokenAndRotates(t *testing.T) {
	mux := newRefreshTestHandler(t, NewMemoryRefreshTokenStore(RefreshConfig{}))
	signup := signupForRefresh(t, mux, "rotate@example.com")
	if signup.RefreshToken == "" {
		t.Fatal("signup did not return a refresh token")
	}
	if signup.RefreshTokenExpiresAt.IsZero() {
		t.Fatal("signup refresh token has zero expiry")
	}

	// Refresh exchanges the token for a fresh access token + a rotated
	// refresh token.
	rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var refreshed AuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &refreshed); err != nil {
		t.Fatalf("decode refresh response: %v", err)
	}
	if refreshed.Token == "" {
		t.Fatal("refresh returned empty access token")
	}
	if refreshed.RefreshToken == "" || refreshed.RefreshToken == signup.RefreshToken {
		t.Fatalf("refresh did not rotate the refresh token (got %q, signup %q)",
			refreshed.RefreshToken, signup.RefreshToken)
	}

	// Replaying the original (now-consumed) token is reuse → 401, and
	// the rotated successor is revoked along with the family.
	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("reuse refresh status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+refreshed.RefreshToken+`"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("post-revoke successor refresh status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshWithoutStoreReturns503(t *testing.T) {
	mux := newRefreshTestHandler(t, nil)
	rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"anything"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("refresh status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

func TestRefreshInvalidAndMalformed(t *testing.T) {
	mux := newRefreshTestHandler(t, NewMemoryRefreshTokenStore(RefreshConfig{}))

	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"bogus-token"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid refresh status = %d, want 401", rec.Code)
	}
	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":""}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty refresh status = %d, want 400", rec.Code)
	}
	if rec := postJSON(t, mux, authPathRefresh, `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed refresh status = %d, want 400", rec.Code)
	}
	// GET is not allowed on the refresh endpoint.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, authPathRefresh, nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET refresh status = %d, want 405", rec.Code)
	}
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	mux := newRefreshTestHandler(t, NewMemoryRefreshTokenStore(RefreshConfig{}))
	signup := signupForRefresh(t, mux, "logout@example.com")

	if rec := postJSON(t, mux, authPathLogout, `{"refreshToken":"`+signup.RefreshToken+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	// The revoked token can no longer be refreshed.
	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh after logout status = %d, want 401", rec.Code)
	}
	// Logout is idempotent: a second logout (and an unknown token)
	// still returns 204.
	if rec := postJSON(t, mux, authPathLogout, `{"refreshToken":"`+signup.RefreshToken+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("double logout status = %d, want 204", rec.Code)
	}
}

func TestLogoutWithoutStoreReturns204(t *testing.T) {
	mux := newRefreshTestHandler(t, nil)
	if rec := postJSON(t, mux, authPathLogout, `{"refreshToken":"anything"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("logout without store status = %d, want 204", rec.Code)
	}
}

func TestRefreshTenantGoneReturns401(t *testing.T) {
	tenants := newFakeTenantStore()
	refresh := NewMemoryRefreshTokenStore(RefreshConfig{})
	h := NewAuthHandler(AuthConfig{
		Tenants:       tenants,
		Auth:          NewMemoryAuthStore(),
		Tokens:        NewMemoryTokenStore(),
		RefreshTokens: refresh,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	signup := signupForRefresh(t, mux, "gone@example.com")
	if err := tenants.DeleteTenant(signup.Tenant.ID); err != nil {
		t.Fatalf("DeleteTenant: %v", err)
	}
	rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh for deleted tenant status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// errRotateStore is a RefreshTokenStore whose Rotate fails with a
// non-sentinel (infrastructure-style) error, standing in for a database
// timeout or commit failure.
type errRotateStore struct{ err error }

func (s errRotateStore) Issue(string) (RefreshToken, error)  { return RefreshToken{}, s.err }
func (s errRotateStore) Rotate(string) (RefreshToken, error) { return RefreshToken{}, s.err }
func (s errRotateStore) Revoke(string) error                 { return s.err }
func (s errRotateStore) RevokeAllForTenant(string) error     { return s.err }

// TestRefreshInfraErrorReturns503 verifies that an infrastructure
// failure from Rotate (not one of the auth sentinels) surfaces as 503
// so the SPA retries the same token rather than being logged out, while
// the sentinel errors still collapse to 401 (covered above).
func TestRefreshInfraErrorReturns503(t *testing.T) {
	mux := newRefreshTestHandler(t, errRotateStore{err: errors.New("dial tcp: connection refused")})
	rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"live-looking-token"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("refresh on infra error status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

// TestLogoutInfraErrorReturns503 verifies that an infrastructure
// failure from Revoke (a DB timeout / connection refused, not a
// validation outcome — Revoke treats unknown/expired tokens as a no-op
// 204) surfaces as 503, matching the refresh handler's retryable status
// for the same fault class rather than a misleading 500. A
// status-specific SPA can then retry so the server-side token is
// actually revoked.
func TestLogoutInfraErrorReturns503(t *testing.T) {
	mux := newRefreshTestHandler(t, errRotateStore{err: errors.New("dial tcp: connection refused")})
	rec := postJSON(t, mux, authPathLogout, `{"refreshToken":"live-looking-token"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("logout on infra error status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
}

// flakyTokenStore issues tokens normally until failAfter successful
// issues, then fails every subsequent IssueToken. It lets a test let
// signup succeed (one issue) and then force the refresh handler's
// IssueToken to fail after Rotate has already consumed the token.
type flakyTokenStore struct {
	inner     *MemoryTokenStore
	issued    int
	failAfter int
}

func (s *flakyTokenStore) IssueToken(tenantID string) (string, error) {
	if s.issued >= s.failAfter {
		return "", errors.New("sign access token: key unavailable")
	}
	s.issued++
	return s.inner.IssueToken(tenantID)
}

func (s *flakyTokenStore) ResolveToken(token string) (string, bool) {
	return s.inner.ResolveToken(token)
}

// TestRefreshIssueTokenFailureReturns401 covers the rare path where
// Rotate succeeds (predecessor consumed, successor minted) but the
// paired access token can't be issued. The presented token is spent, so
// the handler returns 401 (re-authenticate) rather than a misleading
// 500/retry, and the undeliverable successor is revoked — replaying it
// also 401s.
func TestRefreshIssueTokenFailureReturns401(t *testing.T) {
	tokens := &flakyTokenStore{inner: NewMemoryTokenStore(), failAfter: 1}
	refresh := NewMemoryRefreshTokenStore(RefreshConfig{})
	h := NewAuthHandler(AuthConfig{
		Tenants:       newFakeTenantStore(),
		Auth:          NewMemoryAuthStore(),
		Tokens:        tokens,
		RefreshTokens: refresh,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	signup := signupForRefresh(t, mux, "issuefail@example.com")
	// signup consumed the one allowed IssueToken; the refresh below
	// rotates the token fine but then fails to mint the access token.
	rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("refresh on IssueToken failure status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	// The minted-but-undeliverable successor was revoked; any later
	// rotation attempt (predecessor is consumed, successor revoked)
	// also fails closed with 401.
	if rec := postJSON(t, mux, authPathRefresh, `{"refreshToken":"`+signup.RefreshToken+`"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay after IssueToken failure status = %d, want 401", rec.Code)
	}
}
