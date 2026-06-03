package console

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// newMFATestHandler wires an AuthHandler with in-memory stores plus an
// MFA store, driven by the supplied clock so a test can advance time
// across TOTP windows deterministically.
func newMFATestHandler(t *testing.T, mfa MFAStore, clk *fakeClock) *http.ServeMux {
	t.Helper()
	h := NewAuthHandler(AuthConfig{
		Tenants: newFakeTenantStore(),
		Auth:    NewMemoryAuthStore(),
		Tokens:  NewMemoryTokenStore(),
		MFA:     mfa,
		Now:     clk.now,
	})
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// codeForSecret computes the valid TOTP code for secretB32 at time t,
// matching what an authenticator app would show.
func codeForSecret(t *testing.T, secretB32 string, at time.Time) string {
	t.Helper()
	key, err := decodeTOTPSecret(secretB32)
	if err != nil {
		t.Fatalf("decode secret: %v", err)
	}
	return hotpCode(key, totpStep(at.UTC()))
}

const (
	mfaTestEmail = "mfa@example.com"
	mfaTestPass  = "SuperSecretPass123"
)

// signupMFA registers a user so the MFA endpoints have a real tenant to
// authenticate against.
func signupMFA(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	rec := postJSON(t, mux, authPathSignup,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","tenantName":"Acme"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

// enrollAndActivate runs the full enroll → activate flow at the clock's
// current time and returns the shared secret and the issued recovery
// codes. The caller typically advances the clock by one TOTP period
// afterward so a subsequent login is not blocked by the replay guard on
// the activating step.
func enrollAndActivate(t *testing.T, mux *http.ServeMux, clk *fakeClock) (secret string, recoveryCodes []string) {
	t.Helper()
	rec := postJSON(t, mux, authPathMFAEnroll,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var enroll struct {
		Secret     string `json:"secret"`
		OTPAuthURI string `json:"otpauthUri"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("decode enroll: %v", err)
	}
	if enroll.Secret == "" || enroll.OTPAuthURI == "" {
		t.Fatalf("enroll response missing secret/uri: %s", rec.Body.String())
	}

	code := codeForSecret(t, enroll.Secret, clk.now())
	rec = postJSON(t, mux, authPathMFAActivate,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("activate status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var act struct {
		Active        bool     `json:"active"`
		RecoveryCodes []string `json:"recoveryCodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &act); err != nil {
		t.Fatalf("decode activate: %v", err)
	}
	if !act.Active {
		t.Fatal("activate did not report active")
	}
	if len(act.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("got %d recovery codes, want %d", len(act.RecoveryCodes), recoveryCodeCount)
	}
	return enroll.Secret, act.RecoveryCodes
}

func TestMFAEnrollActivateAndLoginStepUp(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)
	secret, _ := enrollAndActivate(t, mux, clk)
	// Move past the activating step so the replay guard does not block
	// a legitimate login that reuses that step's code.
	clk.advance(totpPeriod)

	// Login without a second factor is now rejected with mfaRequired.
	rec := postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login without TOTP status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	var failBody struct {
		MFARequired bool `json:"mfaRequired"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &failBody)
	if !failBody.MFARequired {
		t.Fatalf("login without TOTP should set mfaRequired=true; body = %s", rec.Body.String())
	}

	// A wrong code is also rejected.
	rec = postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"000000"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with wrong TOTP status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}

	// The correct code unlocks login and issues a token.
	code := codeForSecret(t, secret, clk.now())
	rec = postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with TOTP status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var ok AuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &ok); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	if ok.Token == "" {
		t.Fatal("login with valid TOTP returned no access token")
	}
}

func TestMFALoginReplayRejected(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)
	secret, _ := enrollAndActivate(t, mux, clk)
	clk.advance(totpPeriod)

	code := codeForSecret(t, secret, clk.now())
	// First login with the code succeeds.
	rec := postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first login status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	// Replaying the SAME code within its window is rejected: the replay
	// watermark advanced past this step on the first use.
	rec = postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"`+code+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("replay login status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMFARecoveryCodeLoginSingleUse(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)
	_, recovery := enrollAndActivate(t, mux, clk)

	rcode := recovery[0]
	// A recovery code logs the user in once.
	rec := postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","recoveryCode":"`+rcode+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("recovery-code login status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	// The same recovery code cannot be reused.
	rec = postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","recoveryCode":"`+rcode+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("recovery-code reuse status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMFADisableRequiresSecondFactor(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)
	secret, _ := enrollAndActivate(t, mux, clk)

	// Disable with only the password (no second factor) is rejected.
	rec := postJSON(t, mux, authPathMFADisable,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disable without 2FA status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}

	// Disable with a valid TOTP code succeeds. Advance one period so the
	// disable code differs from the activation step (which is fine for
	// disable, but keeps the flow realistic).
	clk.advance(totpPeriod)
	code := codeForSecret(t, secret, clk.now())
	rec = postJSON(t, mux, authPathMFADisable,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`","totpCode":"`+code+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable with TOTP status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// After disable, login no longer requires a second factor.
	rec = postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("post-disable login status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMFAEnrollRejectedWhenAlreadyActive(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)
	enrollAndActivate(t, mux, clk)

	// A second enroll while active must be refused (409) so a working
	// authenticator binding is never silently replaced.
	rec := postJSON(t, mux, authPathMFAEnroll,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("re-enroll while active status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMFAEndpointsReturn503WhenUnconfigured(t *testing.T) {
	// No MFA store wired: management endpoints report 503.
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, nil, clk)
	signupMFA(t, mux)

	for _, path := range []string{authPathMFAEnroll, authPathMFAActivate, authPathMFADisable} {
		rec := postJSON(t, mux, path,
			`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without MFA store status = %d, want 503; body = %s", path, rec.Code, rec.Body.String())
		}
	}

	// And login is single-factor (no MFA enforced).
	rec := postJSON(t, mux, authPathLogin,
		`{"email":"`+mfaTestEmail+`","password":"`+mfaTestPass+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login with no MFA store status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestMFAWrongPasswordRejected(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	mux := newMFATestHandler(t, NewMemoryMFAStore(), clk)
	signupMFA(t, mux)

	// Wrong password on a management endpoint is a uniform 401, never
	// leaking whether MFA exists.
	rec := postJSON(t, mux, authPathMFAEnroll,
		`{"email":"`+mfaTestEmail+`","password":"wrong-password"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("enroll wrong-password status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}
