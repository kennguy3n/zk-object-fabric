package console

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
)

// SQLiteMFAStore is the SQLite-backed MFAStore for the embedded /
// single-node profile. It persists TOTP enrollments so a returning user
// keeps their second factor across a gateway restart, mirroring the
// SQLiteAuthStore and SQLiteRefreshTokenStore in the same embedded
// database.
//
// Concurrency: Activate and the recovery / step mutations read-then-
// write, so they run inside an explicit transaction. A database/sql Tx
// pins one connection, making the read-then-write atomic even though the
// embedded pool would otherwise hand the connection back between
// statements.
type SQLiteMFAStore struct {
	db     *sql.DB
	ctx    context.Context
	sealer SecretSealer
}

// NewSQLiteMFAStore wraps db and creates the MFA tables if they do not
// yet exist. Pass WithSecretSealer to seal the TOTP secret at rest.
func NewSQLiteMFAStore(db *sql.DB, opts ...MFAStoreOption) (*SQLiteMFAStore, error) {
	if db == nil {
		return nil, errors.New("console: sqlite mfa store requires a non-nil *sql.DB")
	}
	s := &SQLiteMFAStore{db: db, sealer: applyMFAStoreOptions(opts).sealer}
	if err := s.ensureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SQLiteMFAStore) ensureSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS mfa_credentials (
			tenant_id  TEXT    PRIMARY KEY,
			secret     TEXT    NOT NULL,
			active     INTEGER NOT NULL DEFAULT 0,
			last_step  INTEGER NOT NULL DEFAULT 0
		)`,
		// The composite primary key (tenant_id, code_hash) already
		// yields a tenant_id-leading index, so the tenant-only lookups
		// (Disable's DELETE, GetMFA's COUNT) ride its leftmost prefix —
		// a separate tenant_id index would be redundant write overhead.
		`CREATE TABLE IF NOT EXISTS mfa_recovery_codes (
			tenant_id  TEXT NOT NULL,
			code_hash  TEXT NOT NULL,
			PRIMARY KEY (tenant_id, code_hash)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("console: ensure mfa schema: %w", err)
		}
	}
	return nil
}

// WithContext returns a copy of the store bound to ctx.
func (s *SQLiteMFAStore) WithContext(ctx context.Context) *SQLiteMFAStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *SQLiteMFAStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

func (s *SQLiteMFAStore) sealSecret(plaintext, tenantID string) (string, error) {
	return sealSecretWith(s.sealer, plaintext, tenantID)
}

func (s *SQLiteMFAStore) openSecret(stored, tenantID string) (string, error) {
	return openSecretWith(s.sealer, stored, tenantID)
}

// BeginEnrollment implements MFAStore.
func (s *SQLiteMFAStore) BeginEnrollment(tenantID, secret string) error {
	if tenantID == "" || secret == "" {
		return errors.New("console: tenantID and secret are required")
	}
	stored, err := s.sealSecret(secret, tenantID)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var active bool
	switch err := tx.QueryRowContext(s.cx(),
		`SELECT active FROM mfa_credentials WHERE tenant_id = ?`, tenantID).Scan(&active); {
	case errors.Is(err, sql.ErrNoRows):
		// no existing row — fall through to insert
	case err != nil:
		return fmt.Errorf("console: load mfa row: %w", err)
	default:
		if active {
			return errMFAAlreadyActive
		}
	}

	// Replace any prior pending secret and clear stale recovery rows
	// (recovery codes only become real at activation).
	if _, err := tx.ExecContext(s.cx(),
		`INSERT INTO mfa_credentials (tenant_id, secret, active, last_step)
		 VALUES (?, ?, 0, 0)
		 ON CONFLICT(tenant_id) DO UPDATE SET secret = excluded.secret, active = 0, last_step = 0`,
		tenantID, stored); err != nil {
		return fmt.Errorf("console: upsert pending mfa: %w", err)
	}
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = ?`, tenantID); err != nil {
		return fmt.Errorf("console: clear stale recovery codes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("console: commit begin enrollment: %w", err)
	}
	return nil
}

// GetMFA implements MFAStore.
func (s *SQLiteMFAStore) GetMFA(tenantID string) (MFARecord, bool, error) {
	var (
		secret   string
		active   bool
		lastStep int64
	)
	switch err := s.db.QueryRowContext(s.cx(),
		`SELECT secret, active, last_step FROM mfa_credentials WHERE tenant_id = ?`, tenantID).
		Scan(&secret, &active, &lastStep); {
	case errors.Is(err, sql.ErrNoRows):
		return MFARecord{}, false, nil
	case err != nil:
		return MFARecord{}, false, fmt.Errorf("console: load mfa: %w", err)
	}
	secret, err := s.openSecret(secret, tenantID)
	if err != nil {
		return MFARecord{}, false, err
	}

	var remaining int
	if err := s.db.QueryRowContext(s.cx(),
		`SELECT COUNT(*) FROM mfa_recovery_codes WHERE tenant_id = ?`, tenantID).
		Scan(&remaining); err != nil {
		return MFARecord{}, false, fmt.Errorf("console: count recovery codes: %w", err)
	}
	return MFARecord{Secret: secret, Active: active, LastStep: lastStep, RecoveryRemaining: remaining}, true, nil
}

// Activate implements MFAStore.
func (s *SQLiteMFAStore) Activate(tenantID, expectedSecret string, firstStep int64, recoveryHashes []string) error {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-read the pending secret inside the transaction and confirm it
	// still matches the secret the caller verified the activating code
	// against. BeginEnrollment replaces a pending secret via upsert, so
	// a concurrent enroll between the handler's GetMFA and this call
	// could otherwise flip active=1 on a row whose secret no longer
	// matches the user's authenticator. The single-connection embedded
	// pool serialises this read-then-write; an already-active or
	// missing row matches nothing (active = 0) and yields
	// errMFANotEnrolled.
	var stored string
	switch err := tx.QueryRowContext(s.cx(),
		`SELECT secret FROM mfa_credentials WHERE tenant_id = ? AND active = 0`, tenantID).
		Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows):
		return errMFANotEnrolled
	case err != nil:
		return fmt.Errorf("console: load pending mfa: %w", err)
	}
	opened, err := s.openSecret(stored, tenantID)
	if err != nil {
		return fmt.Errorf("console: open pending mfa secret: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(opened), []byte(expectedSecret)) != 1 {
		return errMFANotEnrolled
	}

	// "AND active = 0" makes activation atomic: a row that is already
	// active matches zero rows, so a second (racing or repeated)
	// activate returns errMFANotEnrolled instead of clobbering the
	// recovery codes the first activation already handed to the user.
	res, err := tx.ExecContext(s.cx(),
		`UPDATE mfa_credentials SET active = 1, last_step = ? WHERE tenant_id = ? AND active = 0`,
		firstStep, tenantID)
	if err != nil {
		return fmt.Errorf("console: activate mfa: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("console: activate rows affected: %w", err)
	}
	if n == 0 {
		return errMFANotEnrolled
	}

	// Fresh activation replaces any prior recovery set.
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = ?`, tenantID); err != nil {
		return fmt.Errorf("console: clear recovery codes: %w", err)
	}
	for _, h := range recoveryHashes {
		if _, err := tx.ExecContext(s.cx(),
			`INSERT INTO mfa_recovery_codes (tenant_id, code_hash) VALUES (?, ?)`,
			tenantID, h); err != nil {
			return fmt.Errorf("console: insert recovery code: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("console: commit activate: %w", err)
	}
	return nil
}

// MarkTOTPStep implements MFAStore. The UPDATE's WHERE clause is the
// atomic replay guard: it advances last_step only when the presented
// step is strictly newer and the row is active, so a replayed code (step
// <= last_step) affects zero rows and returns ok=false.
func (s *SQLiteMFAStore) MarkTOTPStep(tenantID string, step int64) (bool, error) {
	res, err := s.db.ExecContext(s.cx(),
		`UPDATE mfa_credentials SET last_step = ? WHERE tenant_id = ? AND active = 1 AND last_step < ?`,
		step, tenantID, step)
	if err != nil {
		return false, fmt.Errorf("console: mark totp step: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("console: mark step rows affected: %w", err)
	}
	return n == 1, nil
}

// ConsumeRecoveryCode implements MFAStore. The DELETE is atomic: a hash
// matching an unused code is removed once and returns ok=true; a
// concurrent second attempt with the same code affects zero rows. The
// EXISTS guard requires the enrollment to be active, mirroring
// MemoryMFAStore's `!row.active` check so all three backends agree —
// a recovery code is only ever honoured for an active enrollment.
func (s *SQLiteMFAStore) ConsumeRecoveryCode(tenantID, codeHash string) (bool, error) {
	if codeHash == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = ? AND code_hash = ?
		   AND EXISTS (SELECT 1 FROM mfa_credentials c
		               WHERE c.tenant_id = mfa_recovery_codes.tenant_id AND c.active = 1)`,
		tenantID, codeHash)
	if err != nil {
		return false, fmt.Errorf("console: consume recovery code: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("console: consume rows affected: %w", err)
	}
	return n == 1, nil
}

// Disable implements MFAStore.
func (s *SQLiteMFAStore) Disable(tenantID string) error {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = ?`, tenantID); err != nil {
		return fmt.Errorf("console: delete recovery codes: %w", err)
	}
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_credentials WHERE tenant_id = ?`, tenantID); err != nil {
		return fmt.Errorf("console: delete mfa credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("console: commit disable: %w", err)
	}
	return nil
}

// DisablePending implements MFAStore.
func (s *SQLiteMFAStore) DisablePending(tenantID string) (bool, error) {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return false, fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The active = 0 predicate is the atomic guard: if a concurrent
	// Activate already flipped the row active, this matches zero rows and
	// the active enrollment survives — the handler then falls back to the
	// second-factor-protected disable path.
	res, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_credentials WHERE tenant_id = ? AND active = 0`, tenantID)
	if err != nil {
		return false, fmt.Errorf("console: delete pending mfa credentials: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("console: rows affected: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	// A pending enrollment carries no recovery codes (those are minted at
	// Activate), but clear any defensively so a half-written enrollment
	// cannot strand orphan hashes.
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = ?`, tenantID); err != nil {
		return false, fmt.Errorf("console: delete recovery codes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("console: commit disable pending: %w", err)
	}
	return true, nil
}
