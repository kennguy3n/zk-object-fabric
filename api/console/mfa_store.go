package console

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// DefaultMFAIssuer is the human-facing service name embedded in the
// otpauth:// enrollment URI when AuthConfig.MFAIssuer is left unset. It
// is what the user sees as the account label in their authenticator app.
const DefaultMFAIssuer = "zk-object-fabric"

// recoveryCodeCount is how many single-use recovery codes are minted at
// activation. Ten is the de-facto standard (GitHub, Google) — enough
// that a user who loses their phone can get back in several times before
// regenerating, without being unwieldy to print or store.
const recoveryCodeCount = 10

// recoveryCodeBytes is the entropy per recovery code. Ten bytes (80
// bits) is far beyond guessing for a code that is also rate-limited by
// the login path and invalidated after one use.
const recoveryCodeBytes = 10

// errMFANotEnrolled is returned by Activate when there is no pending
// enrollment to confirm for the tenant. The handler maps it to a 409 so
// a client that calls activate without first calling enroll gets a clear
// "nothing to activate" rather than a misleading success.
var errMFANotEnrolled = errors.New("console: no pending mfa enrollment for tenant")

// errMFAAlreadyActive is returned by BeginEnrollment when the tenant
// already has active MFA. Re-enrolling would silently replace a working
// authenticator binding, so the caller must explicitly Disable first.
var errMFAAlreadyActive = errors.New("console: mfa is already active for tenant")

// MFARecord is the server-side state of a tenant's TOTP enrollment.
// Secret is the base32 shared secret; it is required to verify a code
// and so cannot be hashed at rest the way a password or refresh token
// is — it is the cryptographic equivalent of the symmetric key itself.
// (Encrypting it under the gateway CMK is a possible hardening, deferred
// so this change stays scoped to the MFA protocol; the secret is no more
// sensitive than the access-token signing key already held in the same
// trust boundary.)
type MFARecord struct {
	// Secret is the base32-encoded TOTP shared secret.
	Secret string

	// Active is false for a pending enrollment (secret minted, first
	// code not yet confirmed) and true once the user has proven they
	// can generate a code. Login only enforces MFA when Active.
	Active bool

	// LastStep is the most recent TOTP time step consumed by a
	// successful login. It is the replay guard: a code already used
	// to log in cannot be used again within its still-valid window
	// because MarkTOTPStep only advances forward.
	LastStep int64

	// RecoveryRemaining is the count of unused recovery codes. It is
	// surfaced to the SPA so it can nudge the user to regenerate when
	// they run low; it is not security-sensitive.
	RecoveryRemaining int
}

// MFAStore persists per-tenant TOTP multi-factor state. It is keyed by
// tenant ID, consistent with the RefreshTokenStore and the rest of the
// console (the signup flow binds exactly one user to one tenant). The
// persistent backends (SQLite / Postgres) keep the state in the same
// control-plane database as the AuthStore so MFA is enforced uniformly
// across replicas behind a load balancer — a process-local store would
// let a user dodge MFA by hitting a replica that had not seen their
// enrollment.
type MFAStore interface {
	// BeginEnrollment stores a pending (inactive) TOTP secret for
	// tenantID, replacing any previous pending secret. It returns
	// errMFAAlreadyActive if the tenant already has active MFA, so an
	// existing binding is never silently overwritten — the caller
	// must Disable first.
	BeginEnrollment(tenantID, secret string) error

	// GetMFA returns the tenant's MFA record. ok is false when the
	// tenant has no enrollment (pending or active), which the login
	// path reads as "MFA not required".
	GetMFA(tenantID string) (rec MFARecord, ok bool, err error)

	// Activate confirms a pending enrollment: it flips the record to
	// active, records firstStep as the initial replay watermark (so
	// the code used to activate cannot immediately be replayed to log
	// in), and stores the one-time recovery-code hashes. It returns
	// errMFANotEnrolled when there is no pending row to activate and
	// is a no-op error (not a clobber) when the row is already active.
	Activate(tenantID string, firstStep int64, recoveryHashes []string) error

	// MarkTOTPStep atomically advances the replay watermark: it
	// records step as consumed and returns ok=true only if step is
	// strictly greater than the stored LastStep. A step at or below
	// the watermark (a replayed code) returns ok=false without
	// changing state. ok is false (no error) when the tenant has no
	// active enrollment.
	MarkTOTPStep(tenantID string, step int64) (ok bool, err error)

	// ConsumeRecoveryCode atomically removes one stored recovery hash
	// matching codeHash and returns ok=true. A hash that matches no
	// stored code (already used, never issued, or wrong tenant)
	// returns ok=false without error.
	ConsumeRecoveryCode(tenantID, codeHash string) (ok bool, err error)

	// Disable removes all MFA state for tenantID (pending or active).
	// Disabling a tenant with no enrollment is a no-op, not an error.
	Disable(tenantID string) error
}

// recoveryCodeEncoding is unpadded upper-case base32 (Crockford-free RFC
// 4648), chosen so recovery codes are easy to read aloud / type and have
// no ambiguous padding characters.
var recoveryCodeEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// generateRecoveryCodes mints recoveryCodeCount fresh single-use
// recovery codes and their SHA-256 hex hashes. The plaintext codes are
// returned to the caller to display to the user exactly once; only the
// hashes are persisted, so a database leak cannot recover usable codes —
// the same treatment passwords and refresh tokens get.
func generateRecoveryCodes() (codes []string, hashes []string, err error) {
	codes = make([]string, recoveryCodeCount)
	hashes = make([]string, recoveryCodeCount)
	for i := range codes {
		buf := make([]byte, recoveryCodeBytes)
		if _, err := rand.Read(buf); err != nil {
			return nil, nil, fmt.Errorf("console: generate recovery code: %w", err)
		}
		code := recoveryCodeEncoding.EncodeToString(buf)
		codes[i] = code
		hashes[i] = hashRecoveryCode(code)
	}
	return codes, hashes, nil
}

// hashRecoveryCode normalizes a recovery code (trim + upper-case so a
// hand-typed code with stray case/space still matches) and returns its
// SHA-256 hex digest. The same normalization runs at mint and at consume
// time so the hashes line up.
func hashRecoveryCode(code string) string {
	norm := strings.ToUpper(strings.TrimSpace(code))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

// MemoryMFAStore is a process-local MFAStore for the dev / single-node
// profile and tests. It is not shared across replicas; a multi-node
// deployment must use the SQLite or Postgres backend so MFA is enforced
// everywhere.
type MemoryMFAStore struct {
	mu  sync.Mutex
	rec map[string]*memoryMFARow
}

type memoryMFARow struct {
	secret   string
	active   bool
	lastStep int64
	recovery map[string]struct{} // unused recovery-code hashes
}

// NewMemoryMFAStore returns an empty in-memory MFA store.
func NewMemoryMFAStore() *MemoryMFAStore {
	return &MemoryMFAStore{rec: map[string]*memoryMFARow{}}
}

// BeginEnrollment implements MFAStore.
func (s *MemoryMFAStore) BeginEnrollment(tenantID, secret string) error {
	if tenantID == "" || secret == "" {
		return errors.New("console: tenantID and secret are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if row, ok := s.rec[tenantID]; ok && row.active {
		return errMFAAlreadyActive
	}
	s.rec[tenantID] = &memoryMFARow{secret: secret}
	return nil
}

// GetMFA implements MFAStore.
func (s *MemoryMFAStore) GetMFA(tenantID string) (MFARecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rec[tenantID]
	if !ok {
		return MFARecord{}, false, nil
	}
	return MFARecord{
		Secret:            row.secret,
		Active:            row.active,
		LastStep:          row.lastStep,
		RecoveryRemaining: len(row.recovery),
	}, true, nil
}

// Activate implements MFAStore.
func (s *MemoryMFAStore) Activate(tenantID string, firstStep int64, recoveryHashes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rec[tenantID]
	if !ok {
		return errMFANotEnrolled
	}
	if row.active {
		// Already confirmed: a second activation would clobber the
		// recovery codes the first one returned, leaving the user
		// holding codes that no longer match. Refuse rather than
		// overwrite. This also closes the TOCTOU window between the
		// handler's rec.Active check and this call when two activate
		// requests race on the same pending enrollment.
		return errMFANotEnrolled
	}
	row.active = true
	row.lastStep = firstStep
	row.recovery = make(map[string]struct{}, len(recoveryHashes))
	for _, h := range recoveryHashes {
		row.recovery[h] = struct{}{}
	}
	return nil
}

// MarkTOTPStep implements MFAStore.
func (s *MemoryMFAStore) MarkTOTPStep(tenantID string, step int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rec[tenantID]
	if !ok || !row.active {
		return false, nil
	}
	if step <= row.lastStep {
		return false, nil
	}
	row.lastStep = step
	return true, nil
}

// ConsumeRecoveryCode implements MFAStore.
func (s *MemoryMFAStore) ConsumeRecoveryCode(tenantID, codeHash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rec[tenantID]
	if !ok || !row.active || row.recovery == nil {
		return false, nil
	}
	if _, present := row.recovery[codeHash]; !present {
		return false, nil
	}
	delete(row.recovery, codeHash)
	return true, nil
}

// Disable implements MFAStore.
func (s *MemoryMFAStore) Disable(tenantID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rec, tenantID)
	return nil
}
