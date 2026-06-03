package console

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func newTestStore(t *testing.T, opts func(*JWTConfig)) (*JWTTokenStore, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	cfg := JWTConfig{SigningKey: key, Issuer: "test-issuer"}
	if opts != nil {
		opts(&cfg)
	}
	store, err := NewJWTTokenStore(cfg)
	if err != nil {
		t.Fatalf("NewJWTTokenStore: %v", err)
	}
	return store, key
}

// JWTTokenStore must satisfy the TokenStore interface so it is a
// drop-in replacement for MemoryTokenStore.
var _ TokenStore = (*JWTTokenStore)(nil)

func TestJWTTokenStore_RoundTrip(t *testing.T) {
	store, _ := newTestStore(t, nil)
	token, err := store.IssueToken("t-123")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	got, ok := store.ResolveToken(token)
	if !ok {
		t.Fatalf("ResolveToken returned ok=false for a freshly issued token")
	}
	if got != "t-123" {
		t.Fatalf("ResolveToken tenant = %q, want %q", got, "t-123")
	}
}

func TestJWTTokenStore_IssueRequiresTenant(t *testing.T) {
	store, _ := newTestStore(t, nil)
	if _, err := store.IssueToken(""); err == nil {
		t.Fatal("IssueToken(\"\") should error")
	}
}

func TestJWTTokenStore_DistinctTokensPerIssue(t *testing.T) {
	store, _ := newTestStore(t, nil)
	a, _ := store.IssueToken("t-1")
	b, _ := store.IssueToken("t-1")
	if a == b {
		t.Fatal("two issues for the same tenant produced identical tokens; jti should differ")
	}
}

func TestJWTTokenStore_RejectsEmptyToken(t *testing.T) {
	store, _ := newTestStore(t, nil)
	if _, ok := store.ResolveToken(""); ok {
		t.Fatal("empty token should not resolve")
	}
	if _, ok := store.ResolveToken("not-a-jwt"); ok {
		t.Fatal("garbage token should not resolve")
	}
}

func TestJWTTokenStore_Expiry(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := base
	store, _ := newTestStore(t, func(c *JWTConfig) {
		c.TTL = time.Minute
		c.Now = func() time.Time { return clock }
	})
	token, err := store.IssueToken("t-exp")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	// Still valid just before expiry.
	clock = base.Add(59 * time.Second)
	if _, ok := store.ResolveToken(token); !ok {
		t.Fatal("token should still be valid before expiry")
	}
	// Within the clock-skew leeway window just past TTL the token is
	// intentionally still accepted (absorbs replica clock skew).
	clock = base.Add(time.Minute + 30*time.Second)
	if _, ok := store.ResolveToken(token); !ok {
		t.Fatal("token should still be valid within the clock-skew leeway window")
	}
	// Well past TTL + leeway it must be rejected.
	clock = base.Add(time.Minute + jwtClockSkewLeeway + 10*time.Second)
	if _, ok := store.ResolveToken(token); ok {
		t.Fatal("token should be rejected after expiry + leeway")
	}
}

func TestJWTTokenStore_NotYetValidClockSkew(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	issueClock := base
	store, _ := newTestStore(t, func(c *JWTConfig) {
		c.Now = func() time.Time { return issueClock }
	})
	token, _ := store.IssueToken("t-nbf")

	// A verifier whose clock lags within the leeway window must still
	// accept the token — this is the multi-replica skew case the
	// leeway exists for.
	withinSkew := base.Add(-(jwtClockSkewLeeway - 5*time.Second))
	tolerant, _ := NewJWTTokenStore(JWTConfig{
		SigningKey: storeKey(store),
		Issuer:     "test-issuer",
		Now:        func() time.Time { return withinSkew },
	})
	if _, ok := tolerant.ResolveToken(token); !ok {
		t.Fatal("token used within the clock-skew leeway window should be accepted")
	}

	// A verifier whose clock is well before nbf (beyond leeway) must
	// reject it.
	verifyClock := base.Add(-time.Hour)
	skewed, _ := NewJWTTokenStore(JWTConfig{
		SigningKey: storeKey(store),
		Issuer:     "test-issuer",
		Now:        func() time.Time { return verifyClock },
	})
	if _, ok := skewed.ResolveToken(token); ok {
		t.Fatal("token used an hour before nbf should be rejected")
	}
}

func TestJWTTokenStore_WrongIssuer(t *testing.T) {
	store, key := newTestStore(t, nil)
	token, _ := store.IssueToken("t-iss")
	other, _ := NewJWTTokenStore(JWTConfig{SigningKey: key, Issuer: "different-issuer"})
	if _, ok := other.ResolveToken(token); ok {
		t.Fatal("token minted by a different issuer should be rejected")
	}
}

func TestJWTTokenStore_WrongAudience(t *testing.T) {
	store, key := newTestStore(t, func(c *JWTConfig) { c.Audience = "aud-a" })
	token, _ := store.IssueToken("t-aud")
	other, _ := NewJWTTokenStore(JWTConfig{SigningKey: key, Issuer: "test-issuer", Audience: "aud-b"})
	if _, ok := other.ResolveToken(token); ok {
		t.Fatal("token with a different audience should be rejected")
	}
}

func TestJWTTokenStore_WrongKey(t *testing.T) {
	store, _ := newTestStore(t, nil)
	token, _ := store.IssueToken("t-key")
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := NewJWTTokenStore(JWTConfig{SigningKey: otherKey, Issuer: "test-issuer"})
	if _, ok := other.ResolveToken(token); ok {
		t.Fatal("token verified against the wrong public key should be rejected")
	}
}

func TestJWTTokenStore_TamperedPayload(t *testing.T) {
	store, _ := newTestStore(t, nil)
	token, _ := store.IssueToken("t-orig")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("expected a 3-part JWT, got %d parts", len(parts))
	}
	// Flip the last character of the payload segment; signature no
	// longer matches the altered payload.
	payload := []byte(parts[1])
	payload[len(payload)-1] ^= 0x01
	tampered := parts[0] + "." + string(payload) + "." + parts[2]
	if _, ok := store.ResolveToken(tampered); ok {
		t.Fatal("token with a tampered payload should be rejected")
	}
}

// TestJWTTokenStore_RejectsAlgNone defends against the classic
// alg=none downgrade: a token whose header says alg=none and which
// carries no signature must be rejected, never trusted on the basis
// of its unsigned claims.
func TestJWTTokenStore_RejectsAlgNone(t *testing.T) {
	store, _ := newTestStore(t, nil)
	claims := jwt.RegisteredClaims{
		Subject:   "t-attacker",
		Issuer:    "test-issuer",
		Audience:  jwt.ClaimStrings{DefaultJWTAudience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, claims)
	unsigned, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, ok := store.ResolveToken(unsigned); ok {
		t.Fatal("alg=none token must be rejected")
	}
}

// TestJWTTokenStore_RejectsHMACConfusion defends against the RS256→
// HS256 algorithm-confusion attack: an attacker who knows the RSA
// *public* key (it is public) forges an HS256 token using that public
// key as the HMAC secret. A verifier that does not pin the algorithm
// would hand the public key to the HMAC verifier and accept it.
func TestJWTTokenStore_RejectsHMACConfusion(t *testing.T) {
	store, key := newTestStore(t, nil)
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	claims := jwt.RegisteredClaims{
		Subject:   "t-attacker",
		Issuer:    "test-issuer",
		Audience:  jwt.ClaimStrings{DefaultJWTAudience},
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	forged, err := tok.SignedString(pubPEM)
	if err != nil {
		t.Fatalf("sign HMAC with public key: %v", err)
	}
	if _, ok := store.ResolveToken(forged); ok {
		t.Fatal("HS256 token forged with the RSA public key must be rejected (algorithm confusion)")
	}
}

func TestJWTTokenStore_StatelessAcrossInstances(t *testing.T) {
	store, key := newTestStore(t, nil)
	token, _ := store.IssueToken("t-replica")
	// A second store built from the same key (a different replica
	// behind a load balancer) must validate a token the first one
	// minted — the property MemoryTokenStore lacks.
	replica, err := NewJWTTokenStore(JWTConfig{SigningKey: key, Issuer: "test-issuer"})
	if err != nil {
		t.Fatalf("NewJWTTokenStore replica: %v", err)
	}
	got, ok := replica.ResolveToken(token)
	if !ok || got != "t-replica" {
		t.Fatalf("replica ResolveToken = (%q, %v), want (t-replica, true)", got, ok)
	}
}

func TestNewJWTTokenStore_Validation(t *testing.T) {
	if _, err := NewJWTTokenStore(JWTConfig{Issuer: "x"}); err == nil {
		t.Fatal("nil signing key should error")
	}
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := NewJWTTokenStore(JWTConfig{SigningKey: key}); err == nil {
		t.Fatal("empty issuer should error")
	}
	// Defaults are applied for TTL and audience.
	store, err := NewJWTTokenStore(JWTConfig{SigningKey: key, Issuer: "x"})
	if err != nil {
		t.Fatalf("NewJWTTokenStore: %v", err)
	}
	if store.TokenTTL() != DefaultJWTTokenTTL {
		t.Fatalf("default TTL = %s, want %s", store.TokenTTL(), DefaultJWTTokenTTL)
	}
	if store.audience != DefaultJWTAudience {
		t.Fatalf("default audience = %q, want %q", store.audience, DefaultJWTAudience)
	}
}

func TestLoadRSAPrivateKeyPEM(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	dir := t.TempDir()

	pkcs1 := filepath.Join(dir, "pkcs1.pem")
	pkcs1PEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(pkcs1, pkcs1PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(pkcs1); err != nil {
		t.Fatalf("load PKCS#1: %v", err)
	}

	pkcs8 := filepath.Join(dir, "pkcs8.pem")
	pkcs8DER, _ := x509.MarshalPKCS8PrivateKey(key)
	pkcs8PEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})
	if err := os.WriteFile(pkcs8, pkcs8PEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(pkcs8); err != nil {
		t.Fatalf("load PKCS#8: %v", err)
	}

	// A key loaded from disk must round-trip a token.
	loaded, _ := LoadRSAPrivateKeyPEM(pkcs1)
	store, _ := NewJWTTokenStore(JWTConfig{SigningKey: loaded, Issuer: "disk"})
	tok, _ := store.IssueToken("t-disk")
	if got, ok := store.ResolveToken(tok); !ok || got != "t-disk" {
		t.Fatalf("round-trip from disk key failed: (%q, %v)", got, ok)
	}

	// Error paths.
	if _, err := LoadRSAPrivateKeyPEM(filepath.Join(dir, "missing.pem")); err == nil {
		t.Fatal("missing file should error")
	}
	notPEM := filepath.Join(dir, "notpem.txt")
	if err := os.WriteFile(notPEM, []byte("not pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(notPEM); err == nil {
		t.Fatal("non-PEM file should error")
	}

	// A password-protected key must be rejected with a clear,
	// encryption-specific message rather than a confusing PKCS parse
	// failure. Cover both the legacy OpenSSL (Proc-Type/DEK-Info) and
	// the PKCS#8 ("ENCRYPTED PRIVATE KEY") encodings.
	legacyEnc := filepath.Join(dir, "legacy-enc.pem")
	legacyEncPEM := pem.EncodeToMemory(&pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-256-CBC,0000"},
		Bytes:   []byte("ciphertext"),
	})
	if err := os.WriteFile(legacyEnc, legacyEncPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(legacyEnc); err == nil || !strings.Contains(err.Error(), "password-protected") {
		t.Fatalf("legacy encrypted key: err = %v; want 'password-protected'", err)
	}

	pkcs8Enc := filepath.Join(dir, "pkcs8-enc.pem")
	pkcs8EncPEM := pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: []byte("ciphertext")})
	if err := os.WriteFile(pkcs8Enc, pkcs8EncPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(pkcs8Enc); err == nil || !strings.Contains(err.Error(), "password-protected") {
		t.Fatalf("pkcs8 encrypted key: err = %v; want 'password-protected'", err)
	}

	// A structurally valid but undersized key must be rejected: RS256
	// with a sub-2048-bit modulus is forgeable and unsafe for session
	// tokens.
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	weakPath := filepath.Join(dir, "weak.pem")
	weakPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)})
	if err := os.WriteFile(weakPath, weakPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRSAPrivateKeyPEM(weakPath); err == nil || !strings.Contains(err.Error(), "1024-bit") {
		t.Fatalf("1024-bit key: err = %v; want rejection mentioning '1024-bit'", err)
	}
}

// storeKey extracts the private key from a store for tests that need
// to build a second store from the same key.
func storeKey(s *JWTTokenStore) *rsa.PrivateKey { return s.signKey }
