package console

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// DefaultJWTTokenTTL is the access-token lifetime JWTTokenStore uses
// when AuthConfig leaves TTL unset. One hour balances the SPA's
// session length against the blast radius of a leaked token: a
// stolen token stops working within the hour, and the SPA simply
// re-authenticates (login round-trip) rather than carrying a
// long-lived bearer credential. Operators who front the console with
// a short-lived reverse-proxy session can raise this; public
// deployments should not.
const DefaultJWTTokenTTL = time.Hour

// jwtClockSkewLeeway is the tolerance applied to the time-based
// claims (nbf and exp) during verification. The whole point of a
// stateless token is that any replica behind a load balancer can
// validate it, but those replicas do not share a clock: a verifier
// whose wall clock lags the issuer by a few milliseconds would
// otherwise reject a token it just received as "not yet valid"
// (nbf in the future), and a verifier that runs slightly ahead would
// expire tokens early. A small symmetric leeway absorbs realistic
// NTP-bounded skew without meaningfully extending a token's usable
// life (60s against a 1h TTL is ~1.6%). This is the conventional
// mitigation; it is deliberately small so it cannot mask a grossly
// wrong clock.
const jwtClockSkewLeeway = 60 * time.Second

// jwtSigningMethod pins the algorithm JWTTokenStore signs and accepts.
// It is referenced both when minting (so issued tokens advertise
// RS256) and when verifying (so the parser rejects any token whose
// header names a different alg). Pinning to a single asymmetric
// method is the mitigation for the classic JWT algorithm-confusion
// attacks: a token forged with alg=none or an HMAC token signed with
// the RSA *public* key as the HMAC secret both name a method other
// than RS256 and are rejected before the signature is ever checked.
var jwtSigningMethod = jwt.SigningMethodRS256

// JWTTokenStore is a stateless TokenStore that mints RS256-signed
// JWTs instead of opaque random strings. Where MemoryTokenStore keeps
// a process-local map (so every issued token is lost on restart and
// two gateway replicas behind a load balancer disagree on which
// tokens are valid), a JWT carries its own tenant binding in a
// signed payload: any replica holding the public key can validate a
// token without shared state, and a restart does not invalidate
// outstanding sessions. This is the production replacement the
// TokenStore doc comment calls for.
//
// The store is safe for concurrent use: signing and verification read
// immutable key material and never mutate shared state.
type JWTTokenStore struct {
	// signKey signs newly issued tokens. It is required.
	signKey *rsa.PrivateKey
	// verifyKey validates presented tokens. It is always
	// signKey.Public() for an issuing gateway; kept as a separate
	// field so a future verify-only deployment (one that validates
	// tokens minted elsewhere) can be constructed with just a
	// public key.
	verifyKey *rsa.PublicKey

	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time

	// keyID is the JWT "kid" header stamped on issued tokens and
	// required on presented ones once set. It lets a future
	// deployment rotate signing keys by publishing several public
	// keys and selecting the verifier by kid; today there is a
	// single key so kid is purely informational but stamping it now
	// keeps already-issued tokens forward-compatible with rotation.
	keyID string
}

// JWTConfig configures a JWTTokenStore.
type JWTConfig struct {
	// SigningKey is the RSA private key used to sign tokens. It is
	// required; NewJWTTokenStore returns an error when it is nil.
	SigningKey *rsa.PrivateKey

	// Issuer is stamped as the "iss" claim and required to match on
	// verification. It binds a token to the deployment that minted
	// it so a token issued by one tenant-console cannot be replayed
	// against another that happens to share a signing key.
	Issuer string

	// Audience is stamped as the "aud" claim and required to match
	// on verification. Defaults to "zk-object-fabric-console".
	Audience string

	// TTL is the token lifetime. Defaults to DefaultJWTTokenTTL when
	// non-positive.
	TTL time.Duration

	// KeyID is the optional "kid" header (see JWTTokenStore.keyID).
	KeyID string

	// Now returns the current time. Defaults to time.Now. Tests
	// inject a fixed clock to exercise expiry deterministically.
	Now func() time.Time
}

// DefaultJWTAudience is the "aud" claim used when JWTConfig leaves
// Audience empty.
const DefaultJWTAudience = "zk-object-fabric-console"

// NewJWTTokenStore builds a JWTTokenStore from cfg. It returns an
// error when no signing key or issuer is supplied — both are required
// for a token to be verifiable against a fixed deployment identity.
func NewJWTTokenStore(cfg JWTConfig) (*JWTTokenStore, error) {
	if cfg.SigningKey == nil {
		return nil, errors.New("console: JWT signing key is required")
	}
	if cfg.Issuer == "" {
		return nil, errors.New("console: JWT issuer is required")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = DefaultJWTTokenTTL
	}
	aud := cfg.Audience
	if aud == "" {
		aud = DefaultJWTAudience
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &JWTTokenStore{
		signKey:   cfg.SigningKey,
		verifyKey: &cfg.SigningKey.PublicKey,
		issuer:    cfg.Issuer,
		audience:  aud,
		ttl:       ttl,
		now:       now,
		keyID:     cfg.KeyID,
	}, nil
}

// IssueToken mints an RS256-signed JWT binding tenantID into the
// "sub" claim. The token carries iss/aud/iat/nbf/exp and a random
// "jti" so two tokens minted for the same tenant in the same instant
// are still distinct (useful for audit correlation and for a future
// deny-list keyed on jti).
func (s *JWTTokenStore) IssueToken(tenantID string) (string, error) {
	if tenantID == "" {
		return "", errors.New("console: tenant_id is required to issue a token")
	}
	jti, err := randomTokenID()
	if err != nil {
		return "", err
	}
	now := s.now()
	claims := jwt.RegisteredClaims{
		Subject:   tenantID,
		Issuer:    s.issuer,
		Audience:  jwt.ClaimStrings{s.audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		ID:        jti,
	}
	tok := jwt.NewWithClaims(jwtSigningMethod, claims)
	if s.keyID != "" {
		tok.Header["kid"] = s.keyID
	}
	signed, err := tok.SignedString(s.signKey)
	if err != nil {
		return "", fmt.Errorf("console: sign JWT: %w", err)
	}
	return signed, nil
}

// ResolveToken verifies a presented JWT and returns the bound tenant
// ID. It returns ("", false) for any token that fails signature
// verification, names a non-RS256 algorithm, is expired / not yet
// valid, or carries the wrong issuer or audience. The boolean-only
// failure signal matches the TokenStore contract (and MemoryTokenStore's
// behaviour): callers translate a false result into a 401 without
// learning *why* the token was rejected, so a probing client cannot
// distinguish "expired" from "forged".
func (s *JWTTokenStore) ResolveToken(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	claims := &jwt.RegisteredClaims{}
	parsed, err := jwt.ParseWithClaims(token, claims,
		func(t *jwt.Token) (any, error) {
			// Defence in depth: WithValidMethods below already
			// rejects a wrong alg before this keyfunc runs, but
			// re-checking the concrete method here means a forged
			// HMAC token can never reach a code path that would
			// hand the RSA public key to an HMAC verifier.
			if t.Method != jwtSigningMethod {
				return nil, fmt.Errorf("console: unexpected signing method %q", t.Header["alg"])
			}
			return s.verifyKey, nil
		},
		jwt.WithValidMethods([]string{jwtSigningMethod.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(s.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(jwtClockSkewLeeway),
		jwt.WithTimeFunc(s.now),
	)
	if err != nil || !parsed.Valid {
		return "", false
	}
	if claims.Subject == "" {
		return "", false
	}
	return claims.Subject, true
}

// TokenTTL returns the lifetime stamped on issued tokens. Exposed so
// the gateway can log the effective value at startup.
func (s *JWTTokenStore) TokenTTL() time.Duration { return s.ttl }

// randomTokenID returns a 16-byte URL-safe random identifier for the
// "jti" claim.
func randomTokenID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand jti: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// LoadRSAPrivateKeyPEM reads a PEM-encoded RSA private key from path.
// It accepts both PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE
// KEY") encodings — the two forms `openssl genrsa` and `openssl
// genpkey` emit — so an operator does not have to know which tool
// produced the file. A PKCS#8 key that is not RSA is rejected, since
// JWTTokenStore only signs RS256.
func LoadRSAPrivateKeyPEM(path string) (*rsa.PrivateKey, error) {
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("console: read JWT signing key %q: %w", path, err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("console: JWT signing key %q is not PEM-encoded", path)
	}
	key, pkcs1Err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if pkcs1Err == nil {
		return key, nil
	}
	parsed, pkcs8Err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if pkcs8Err != nil {
		// Surface both parse failures: when an operator hands us a
		// corrupted PKCS#1 file the PKCS#8 error alone is misleading
		// (it complains about the wrong format), so report each.
		return nil, fmt.Errorf("console: parse JWT signing key %q (PKCS#1: %v; PKCS#8: %v)", path, pkcs1Err, pkcs8Err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("console: JWT signing key %q is %T, want *rsa.PrivateKey", path, parsed)
	}
	return rsaKey, nil
}
