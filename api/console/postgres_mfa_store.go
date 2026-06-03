package console

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// PostgresMFAStore is the Postgres-backed MFAStore for the multi-replica
// production profile. It persists TOTP enrollments in the mfa_credentials
// and mfa_recovery_codes tables defined in api/console/schema.sql, so
// every gateway replica behind a load balancer enforces the same second
// factor — a process-local store would let a user dodge MFA by landing
// on a replica that had not seen their enrollment.
//
// Like PostgresAuthStore it does not create its own schema; operators run
// schema.sql before the first query.
type PostgresMFAStore struct {
	db  *sql.DB
	ctx context.Context
}

// NewPostgresMFAStore wraps db.
func NewPostgresMFAStore(db *sql.DB) (*PostgresMFAStore, error) {
	if db == nil {
		return nil, errors.New("console: postgres mfa store requires a non-nil *sql.DB")
	}
	return &PostgresMFAStore{db: db}, nil
}

// WithContext returns a copy of the store bound to ctx.
func (s *PostgresMFAStore) WithContext(ctx context.Context) *PostgresMFAStore {
	clone := *s
	clone.ctx = ctx
	return &clone
}

func (s *PostgresMFAStore) cx() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// BeginEnrollment implements MFAStore. The SELECT ... FOR UPDATE
// serialises a concurrent enroll / activate on the same tenant so the
// active-row check and the upsert see a consistent state.
func (s *PostgresMFAStore) BeginEnrollment(tenantID, secret string) error {
	if tenantID == "" || secret == "" {
		return errors.New("console: tenantID and secret are required")
	}
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var active bool
	switch err := tx.QueryRowContext(s.cx(),
		`SELECT active FROM mfa_credentials WHERE tenant_id = $1 FOR UPDATE`, tenantID).Scan(&active); {
	case errors.Is(err, sql.ErrNoRows):
		// no existing row — fall through to insert
	case err != nil:
		return fmt.Errorf("console: load mfa row: %w", err)
	default:
		if active {
			return errMFAAlreadyActive
		}
	}

	if _, err := tx.ExecContext(s.cx(),
		`INSERT INTO mfa_credentials (tenant_id, secret, active, last_step)
		 VALUES ($1, $2, FALSE, 0)
		 ON CONFLICT (tenant_id) DO UPDATE SET secret = EXCLUDED.secret, active = FALSE, last_step = 0`,
		tenantID, secret); err != nil {
		return fmt.Errorf("console: upsert pending mfa: %w", err)
	}
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("console: clear stale recovery codes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("console: commit begin enrollment: %w", err)
	}
	return nil
}

// GetMFA implements MFAStore.
func (s *PostgresMFAStore) GetMFA(tenantID string) (MFARecord, bool, error) {
	var (
		secret   string
		active   bool
		lastStep int64
	)
	switch err := s.db.QueryRowContext(s.cx(),
		`SELECT secret, active, last_step FROM mfa_credentials WHERE tenant_id = $1`, tenantID).
		Scan(&secret, &active, &lastStep); {
	case errors.Is(err, sql.ErrNoRows):
		return MFARecord{}, false, nil
	case err != nil:
		return MFARecord{}, false, fmt.Errorf("console: load mfa: %w", err)
	}

	var remaining int
	if err := s.db.QueryRowContext(s.cx(),
		`SELECT COUNT(*) FROM mfa_recovery_codes WHERE tenant_id = $1`, tenantID).
		Scan(&remaining); err != nil {
		return MFARecord{}, false, fmt.Errorf("console: count recovery codes: %w", err)
	}
	return MFARecord{Secret: secret, Active: active, LastStep: lastStep, RecoveryRemaining: remaining}, true, nil
}

// Activate implements MFAStore.
func (s *PostgresMFAStore) Activate(tenantID string, firstStep int64, recoveryHashes []string) error {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// "AND active = FALSE" makes activation atomic: a row that is
	// already active matches zero rows, so a second (racing or
	// repeated) activate returns errMFANotEnrolled instead of
	// clobbering the recovery codes the first activation already handed
	// to the user.
	res, err := tx.ExecContext(s.cx(),
		`UPDATE mfa_credentials SET active = TRUE, last_step = $1 WHERE tenant_id = $2 AND active = FALSE`,
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

	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("console: clear recovery codes: %w", err)
	}
	for _, h := range recoveryHashes {
		if _, err := tx.ExecContext(s.cx(),
			`INSERT INTO mfa_recovery_codes (tenant_id, code_hash) VALUES ($1, $2)`,
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
// atomic replay guard: it advances last_step only when step is strictly
// newer and the row is active, so a replayed code affects zero rows.
func (s *PostgresMFAStore) MarkTOTPStep(tenantID string, step int64) (bool, error) {
	res, err := s.db.ExecContext(s.cx(),
		`UPDATE mfa_credentials SET last_step = $1 WHERE tenant_id = $2 AND active = TRUE AND last_step < $1`,
		step, tenantID)
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
// matching an unused code is removed once; a concurrent replay of the
// same code affects zero rows.
func (s *PostgresMFAStore) ConsumeRecoveryCode(tenantID, codeHash string) (bool, error) {
	if codeHash == "" {
		return false, nil
	}
	res, err := s.db.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = $1 AND code_hash = $2`,
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
func (s *PostgresMFAStore) Disable(tenantID string) error {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("console: delete recovery codes: %w", err)
	}
	if _, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_credentials WHERE tenant_id = $1`, tenantID); err != nil {
		return fmt.Errorf("console: delete mfa credentials: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("console: commit disable: %w", err)
	}
	return nil
}

// DisablePending implements MFAStore.
func (s *PostgresMFAStore) DisablePending(tenantID string) (bool, error) {
	tx, err := s.db.BeginTx(s.cx(), nil)
	if err != nil {
		return false, fmt.Errorf("console: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The active = FALSE predicate is the atomic guard: if a concurrent
	// Activate already flipped the row active, this matches zero rows and
	// the active enrollment survives — the handler then falls back to the
	// second-factor-protected disable path.
	res, err := tx.ExecContext(s.cx(),
		`DELETE FROM mfa_credentials WHERE tenant_id = $1 AND active = FALSE`, tenantID)
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
		`DELETE FROM mfa_recovery_codes WHERE tenant_id = $1`, tenantID); err != nil {
		return false, fmt.Errorf("console: delete recovery codes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("console: commit disable pending: %w", err)
	}
	return true, nil
}
