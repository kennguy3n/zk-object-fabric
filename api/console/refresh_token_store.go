package console

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

// DefaultRefreshTokenTTL is the lifetime a refresh token is given when
// RefreshConfig leaves TTL unset. Thirty days lets a returning SPA user
// skip re-entering credentials for a month while still bounding how long
// a leaked-but-undetected refresh token stays usable. It is deliberately
// far longer than the access token's one-hour life (jwt_token_store.go):
// the access token is the thing presented on every request, so it is
// short-lived to keep the blast radius small, while the refresh token is
// presented only at the /auth/refresh endpoint and is rotated on every
// use (see RefreshTokenStore).
const DefaultRefreshTokenTTL = 30 * 24 * time.Hour

// errRefreshTokenInvalid is returned by Rotate/Revoke for a token that
// is unknown, malformed, or expired. Like ResolveToken's boolean-only
// failure, the handler maps it to a uniform 401 so a probing client
// cannot distinguish "never existed" from "expired".
var errRefreshTokenInvalid = errors.New("console: refresh token is invalid or expired")

// errRefreshTokenReuse is returned by Rotate when a token that has
// already been rotated away is presented again. A correct client never
// reuses a refresh token (it always swaps in the successor Rotate
// returned), so a replay means the token leaked and both the attacker
// and the legitimate client now hold copies of the same family. The
// store reacts by revoking the entire family — every outstanding token
// descended from the original Issue — which logs the real user out but
// denies the thief continued access. The handler treats this the same
// as errRefreshTokenInvalid on the wire (uniform 401) so the reuse
// signal is not leaked to the caller.
var errRefreshTokenReuse = errors.New("console: refresh token reuse detected; family revoked")

// RefreshToken is the result of issuing or rotating a refresh token.
// Raw is the opaque secret handed to the client exactly once — it is
// never stored server-side (only its hash is), so it cannot be recovered
// after this struct is returned.
type RefreshToken struct {
	Raw       string
	TenantID  string
	ExpiresAt time.Time
}

// RefreshTokenStore persists the long-lived refresh tokens the SPA
// exchanges for fresh short-lived access tokens without re-entering
// credentials. Unlike the stateless access-token JWT
// (jwt_token_store.go), refresh tokens are stateful by necessity:
// rotation-on-use and stolen-token reuse detection both require the
// server to remember which token is currently live for a session, which
// a self-contained signed token cannot express. The persistent backends
// (SQLite / Postgres) keep that state in the same control-plane database
// the AuthStore uses, so it is shared across replicas behind a load
// balancer.
//
// Only a SHA-256 hash of each token is stored; the raw secret lives
// solely in the RefreshToken returned to the caller. A database leak
// therefore exposes no usable tokens.
type RefreshTokenStore interface {
	// Issue mints a new refresh token for tenantID, starting a fresh
	// rotation family. The raw token is returned exactly once.
	Issue(tenantID string) (RefreshToken, error)

	// Rotate validates rawToken and, on success, atomically marks it
	// consumed and returns a freshly minted successor in the same
	// family. A valid-but-already-consumed token triggers reuse
	// handling: the whole family is revoked and errRefreshTokenReuse
	// is returned. Unknown / malformed / expired tokens return
	// errRefreshTokenInvalid.
	Rotate(rawToken string) (RefreshToken, error)

	// Revoke invalidates a single refresh token (logout on one
	// device). Revoking an unknown token is a no-op, not an error,
	// so a double logout does not 500.
	Revoke(rawToken string) error

	// RevokeAllForTenant invalidates every refresh token belonging to
	// tenantID (logout-everywhere, or a forced invalidation after a
	// password reset). Returns nil when the tenant has no tokens.
	RevokeAllForTenant(tenantID string) error
}

// RefreshConfig configures a refresh-token store's lifetime and clock.
type RefreshConfig struct {
	// TTL is the refresh-token lifetime. Defaults to
	// DefaultRefreshTokenTTL when non-positive.
	TTL time.Duration

	// Now returns the current time. Defaults to time.Now; tests inject
	// a fixed clock to exercise expiry deterministically.
	Now func() time.Time
}

func (c RefreshConfig) ttl() time.Duration {
	if c.TTL <= 0 {
		return DefaultRefreshTokenTTL
	}
	return c.TTL
}

func (c RefreshConfig) now() func() time.Time {
	if c.Now == nil {
		return time.Now
	}
	return c.Now
}

// refreshTokenBytes is the entropy of a raw refresh token. 32 bytes
// (256 bits) is well beyond brute-force reach and matches the
// MemoryTokenStore opaque-token width.
const refreshTokenBytes = 32

// refreshFamilyBytes is the entropy of a family identifier. The family
// ID is not a secret (it only groups a token lineage for reuse
// revocation), but 16 random bytes keep it collision-free across any
// realistic number of sessions.
const refreshFamilyBytes = 16

// newRawRefreshToken mints a fresh opaque refresh token.
func newRawRefreshToken() (string, error) {
	buf := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand refresh token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// newRefreshFamilyID mints a fresh family identifier.
func newRefreshFamilyID() (string, error) {
	buf := make([]byte, refreshFamilyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: rand refresh family: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// hashRefreshToken returns the hex-encoded SHA-256 of a raw refresh
// token. The store persists only this digest, so the raw token cannot
// be recovered from a database dump. SHA-256 (not bcrypt) is correct
// here: the input is 256 bits of uniform random entropy, so it is not
// dictionary-attackable, and refresh lookups must be fast and indexable.
func hashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// MemoryRefreshTokenStore is a process-local RefreshTokenStore for the
// dev / test profile. Like MemoryTokenStore it loses all tokens on
// restart and is not shared across replicas, so production wires the
// SQLite or Postgres backend instead.
type MemoryRefreshTokenStore struct {
	cfg RefreshConfig
	mu  sync.Mutex
	// rows is keyed by token hash. Storing the hash (not the raw
	// token) keeps the in-memory store consistent with the persistent
	// backends and means a heap dump never reveals a usable token.
	rows map[string]memoryRefreshRow
}

type memoryRefreshRow struct {
	tenantID  string
	familyID  string
	expiresAt time.Time
	consumed  bool
}

// NewMemoryRefreshTokenStore returns an empty store.
func NewMemoryRefreshTokenStore(cfg RefreshConfig) *MemoryRefreshTokenStore {
	return &MemoryRefreshTokenStore{cfg: cfg, rows: map[string]memoryRefreshRow{}}
}

// Issue implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) Issue(tenantID string) (RefreshToken, error) {
	if tenantID == "" {
		return RefreshToken{}, errors.New("console: tenant_id is required to issue a refresh token")
	}
	family, err := newRefreshFamilyID()
	if err != nil {
		return RefreshToken{}, err
	}
	return s.mint(tenantID, family)
}

// Rotate implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) Rotate(rawToken string) (RefreshToken, error) {
	if rawToken == "" {
		return RefreshToken{}, errRefreshTokenInvalid
	}
	hash := hashRefreshToken(rawToken)
	now := s.cfg.now()()

	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[hash]
	if !ok {
		return RefreshToken{}, errRefreshTokenInvalid
	}
	if !now.Before(row.expiresAt) {
		delete(s.rows, hash)
		return RefreshToken{}, errRefreshTokenInvalid
	}
	if row.consumed {
		// Reuse of an already-rotated token: revoke the whole
		// family so neither the thief nor the victim can continue.
		s.revokeFamilyLocked(row.familyID)
		return RefreshToken{}, errRefreshTokenReuse
	}
	row.consumed = true
	s.rows[hash] = row
	return s.mintLocked(row.tenantID, row.familyID, now)
}

// Revoke implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) Revoke(rawToken string) error {
	if rawToken == "" {
		return nil
	}
	hash := hashRefreshToken(rawToken)
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, hash)
	return nil
}

// RevokeAllForTenant implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) RevokeAllForTenant(tenantID string) error {
	if tenantID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for hash, row := range s.rows {
		if row.tenantID == tenantID {
			delete(s.rows, hash)
		}
	}
	return nil
}

func (s *MemoryRefreshTokenStore) mint(tenantID, familyID string) (RefreshToken, error) {
	now := s.cfg.now()()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mintLocked(tenantID, familyID, now)
}

// mintLocked inserts a fresh token row and returns it. Callers must hold
// s.mu.
func (s *MemoryRefreshTokenStore) mintLocked(tenantID, familyID string, now time.Time) (RefreshToken, error) {
	raw, err := newRawRefreshToken()
	if err != nil {
		return RefreshToken{}, err
	}
	expiresAt := now.Add(s.cfg.ttl())
	s.rows[hashRefreshToken(raw)] = memoryRefreshRow{
		tenantID:  tenantID,
		familyID:  familyID,
		expiresAt: expiresAt,
	}
	return RefreshToken{Raw: raw, TenantID: tenantID, ExpiresAt: expiresAt}, nil
}

// revokeFamilyLocked deletes every row in familyID. Callers must hold
// s.mu.
func (s *MemoryRefreshTokenStore) revokeFamilyLocked(familyID string) {
	for hash, row := range s.rows {
		if row.familyID == familyID {
			delete(s.rows, hash)
		}
	}
}
