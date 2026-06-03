// Package-internal Postgres-backed multipart store.
//
// The Phase 2 multipart store (NewMemoryStore) lives in process
// memory: it is a single-node, no-durability primitive. The
// production deployment shape needs a multipart record that
// survives gateway restarts and lets a client complete an upload
// against a different node than the one that handled
// CreateMultipartUpload — that's what this file ships.
//
// On-disk layout (one row per upload + one row per part):
//
//	CREATE TABLE multipart_uploads (
//	    upload_id           TEXT PRIMARY KEY,
//	    tenant_id           TEXT NOT NULL,
//	    bucket              TEXT NOT NULL,
//	    object_key          TEXT NOT NULL,
//	    version_id          TEXT,
//	    backend             TEXT,
//	    policy              JSONB NOT NULL,
//	    enc_mode            TEXT,
//	    wrapped_dek         BYTEA,
//	    wrapped_key_id      TEXT,
//	    wrap_algorithm      TEXT,
//	    content_algorithm   TEXT,
//	    created_at          TIMESTAMPTZ NOT NULL DEFAULT now()
//	);
//	CREATE INDEX multipart_uploads_by_tenant_bucket
//	    ON multipart_uploads (tenant_id, bucket);
//	CREATE INDEX multipart_uploads_by_created_at
//	    ON multipart_uploads (created_at);
//
//	CREATE TABLE multipart_parts (
//	    upload_id           TEXT NOT NULL
//	                          REFERENCES multipart_uploads(upload_id)
//	                          ON DELETE CASCADE,
//	    tenant_id           TEXT,  -- denormalised from the owning
//	                               -- upload so the uniform RLS policy
//	                               -- keys on it (see rls.sql)
//	    part_number         INTEGER NOT NULL,
//	    piece_id            TEXT NOT NULL,
//	    backend             TEXT NOT NULL,
//	    etag                TEXT,
//	    size_bytes          BIGINT,
//	    part_hash           BYTEA,
//	    plaintext_part_hash BYTEA,
//	    uploaded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
//	    PRIMARY KEY (upload_id, part_number)
//	);
//
// DEKMaterial — the plaintext per-object DEK held in-memory for the
// duration of a managed / public_distribution multipart session —
// is NEVER persisted by this store. The row holds only the
// CMK-wrapped form (wrapped_dek). Get unwraps that back into
// DEKMaterial using the gateway-supplied Wrapper so the multipart
// handler can seal individual parts on subsequent UploadPart calls
// without re-asking the CMK each time.
//
// A small in-memory session cache keeps the same *Upload pointer
// alive across Get calls within a single process so the multipart
// handler's existing pointer-mutation pattern (SetPartHash /
// SetPlaintextPartHash) keeps working. Persistence of part hashes
// across nodes is handled by storing them on the multipart_parts
// row.

package multipart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/internal/rlsdb"
)

// rowQuerier is the read surface shared by *sql.DB and *sql.Tx.
// loadUpload / loadParts take it so the same scan code runs both
// inside a tenant-bound transaction (request paths) and inside a
// scan_all transaction (the expiry sweeper).
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// DefaultUploadTTL is the default time-to-live for an in-flight
// multipart upload before the expiry sweeper deletes it. Mirrors
// the S3 default and AWS recommendation (24h). Operators can
// shorten this via Config.UploadTTL if they want to free orphaned
// part storage sooner.
const DefaultUploadTTL = 24 * time.Hour

// DefaultExpirySweepInterval is how often the expiry goroutine
// scans the table for uploads past UploadTTL. The cadence balances
// freshness against database load — at 10 minutes the sweeper
// processes at most one tick per upload's lifetime per minute of
// over-TTL age.
const DefaultExpirySweepInterval = 10 * time.Minute

// CleanupFunc is invoked once per expired upload before the row
// is deleted. The gateway main passes a closure that fans out
// DeletePiece calls to each part's backend so abandoned ciphertext
// does not accumulate on storage providers. The store passes a
// background-derived context with a short timeout so cleanup
// blockers cannot stall the sweeper indefinitely.
type CleanupFunc func(ctx context.Context, upload *Upload, parts []Part)

// PostgresConfig wires the Postgres store to its dependencies. The
// store does not register a driver — callers must
// _ "github.com/lib/pq" (or pgx) in their main package and hand the
// store an *sql.DB they have already pinged.
type PostgresConfig struct {
	// DB is the shared metadata pool (same one the manifest store
	// and content-index store use). Required.
	DB *sql.DB

	// UploadsTable defaults to "multipart_uploads". Provided so
	// operators with naming policies can override; the value must
	// pass isSafeMultipartIdent.
	UploadsTable string

	// PartsTable defaults to "multipart_parts". Same naming rules
	// as UploadsTable.
	PartsTable string

	// Wrapper unwraps wrapped_dek back into DEKMaterial on Get.
	// Required when any upload uses gateway-side encryption
	// ("managed" or "public_distribution"); nil is acceptable in
	// dev / test setups that only exercise the "" or
	// "client_side" modes (which carry no DEKMaterial).
	Wrapper client_sdk.Wrapper

	// CMK is the customer master key reference the Wrapper uses
	// to validate the wrap envelope. Must match what the
	// multipart handler used at CreateMultipartUpload time.
	CMK encryption.CustomerMasterKeyRef

	// UploadTTL caps how long an in-flight upload may live before
	// the expiry sweeper deletes it. Defaults to DefaultUploadTTL.
	UploadTTL time.Duration

	// ExpirySweepInterval controls how often the sweeper runs.
	// Defaults to DefaultExpirySweepInterval. Tests override it to
	// a short value so they don't have to sleep 10 minutes.
	ExpirySweepInterval time.Duration

	// Cleanup, when non-nil, is invoked for every upload the
	// sweeper expires before the row is deleted. Used by the
	// gateway to fan out DeletePiece calls so abandoned parts
	// don't accumulate on the backend.
	Cleanup CleanupFunc

	// Logger receives sweeper diagnostic output. nil discards.
	Logger *log.Logger

	// Clock is the time source the store uses for CreatedAt and
	// the expiry comparison. nil uses time.Now.
	Clock func() time.Time
}

// PostgresStore is a Postgres-backed Store. It is safe for
// concurrent use by the HTTP handler and by the expiry sweeper
// goroutine.
type PostgresStore struct {
	db                   *sql.DB
	uploadsTable         string
	partsTable           string
	wrapper              client_sdk.Wrapper
	cmk                  encryption.CustomerMasterKeyRef
	uploadTTL            time.Duration
	expirySweepInterval  time.Duration
	cleanup              CleanupFunc
	logger               *log.Logger
	clock                func() time.Time
	stopSweep            chan struct{}
	sweepDone            chan struct{}

	// sessions caches the *Upload pointer returned by Get so the
	// multipart handler's existing pattern of mutating PartHashes
	// / PlaintextPartHashes between Get and PutPart keeps working
	// within a single process. Cross-node correctness for the
	// hashes themselves is handled by persisting them on the
	// multipart_parts row.
	sessions sync.Map // map[string]*Upload
}

// NewPostgresStore returns a configured PostgresStore. It does not
// open or migrate the database; callers are responsible for
// running the schema (see the package doc). The expiry sweeper
// goroutine starts immediately and runs until Close.
func NewPostgresStore(cfg PostgresConfig) (*PostgresStore, error) {
	if cfg.DB == nil {
		return nil, errors.New("multipart: postgres store requires a non-nil *sql.DB")
	}
	uploads := cfg.UploadsTable
	if uploads == "" {
		uploads = "multipart_uploads"
	}
	if !isSafeMultipartIdent(uploads) {
		return nil, fmt.Errorf("multipart: invalid uploads table name %q", uploads)
	}
	parts := cfg.PartsTable
	if parts == "" {
		parts = "multipart_parts"
	}
	if !isSafeMultipartIdent(parts) {
		return nil, fmt.Errorf("multipart: invalid parts table name %q", parts)
	}
	ttl := cfg.UploadTTL
	if ttl <= 0 {
		ttl = DefaultUploadTTL
	}
	sweep := cfg.ExpirySweepInterval
	if sweep <= 0 {
		sweep = DefaultExpirySweepInterval
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	s := &PostgresStore{
		db:                   cfg.DB,
		uploadsTable:         uploads,
		partsTable:           parts,
		wrapper:              cfg.Wrapper,
		cmk:                  cfg.CMK,
		uploadTTL:            ttl,
		expirySweepInterval:  sweep,
		cleanup:              cfg.Cleanup,
		logger:               cfg.Logger,
		clock:                clock,
		stopSweep:            make(chan struct{}),
		sweepDone:            make(chan struct{}),
	}
	go s.runExpirySweeper()
	return s, nil
}

// Close stops the expiry sweeper. The underlying *sql.DB is owned
// by the caller and is NOT closed here.
func (s *PostgresStore) Close() error {
	select {
	case <-s.stopSweep:
		// Already closed.
	default:
		close(s.stopSweep)
	}
	<-s.sweepDone
	return nil
}

// Create inserts a new upload row plus its (empty) cached *Upload
// pointer. The plaintext DEKMaterial is NOT persisted; only the
// CMK-wrapped form (upload.WrappedDEK) is stored.
func (s *PostgresStore) Create(upload *Upload) error {
	if upload == nil || upload.ID == "" {
		return errors.New("multipart: upload id is required")
	}
	policy, err := json.Marshal(upload.Policy)
	if err != nil {
		return fmt.Errorf("multipart: marshal policy: %w", err)
	}
	if upload.CreatedAt.IsZero() {
		upload.CreatedAt = s.clock()
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (
			upload_id, tenant_id, bucket, object_key, version_id, backend,
			policy, enc_mode,
			wrapped_dek, wrapped_key_id, wrap_algorithm, content_algorithm,
			created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
	`, s.uploadsTable)
	// Bind the upload's tenant so the RLS WITH CHECK clause admits the
	// INSERT (it rejects any row whose tenant_id != the bound GUC).
	tx, err := rlsdb.BeginTenant(context.Background(), s.db, upload.TenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(), q,
		upload.ID,
		upload.TenantID,
		upload.Bucket,
		upload.ObjectKey,
		nullableString(upload.VersionID),
		upload.Backend,
		policy,
		upload.EncMode,
		nullableBytes(upload.WrappedDEK),
		nullableString(upload.WrappedKeyID),
		nullableString(upload.WrapAlgorithm),
		nullableString(upload.ContentAlgorithm),
		upload.CreatedAt,
	); err != nil {
		return fmt.Errorf("multipart: insert upload: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("multipart: commit create: %w", err)
	}

	// Initialise the in-process session cache so subsequent Get
	// calls within the same process return the same pointer; the
	// handler's PartHashes / PlaintextPartHashes mutations between
	// Get and PutPart stay observable across the upload's
	// lifetime. The cached upload aliases the caller-supplied
	// pointer (its parts map is initialised here, matching
	// MemoryStore.Create semantics).
	upload.parts = map[int]Part{}
	if upload.PartHashes == nil {
		upload.PartHashes = make(map[int][]byte)
	}
	if upload.PlaintextPartHashes == nil {
		upload.PlaintextPartHashes = make(map[int][]byte)
	}
	s.sessions.Store(upload.ID, upload)
	return nil
}

// Get returns the upload by ID. The first call loads the row +
// parts from Postgres and caches the *Upload pointer; subsequent
// calls within the same process return the cached pointer so the
// multipart handler's pointer-mutation pattern keeps working.
//
// Unwrapping the DEK requires the Config.Wrapper; uploads written
// with gateway-side encryption against a store missing a Wrapper
// surface a clear error rather than silently returning an empty
// DEKMaterial.
func (s *PostgresStore) Get(tenantID, uploadID string) (*Upload, error) {
	if cached, ok := s.sessions.Load(uploadID); ok {
		u := cached.(*Upload)
		// Defence-in-depth: the cache is keyed by upload_id alone, so a
		// cached pointer could belong to another tenant. Cross-check the
		// owner and treat a mismatch as a miss (ErrNotFound), mirroring
		// the RLS query below — a cross-tenant caller must not even learn
		// the upload exists.
		if u.TenantID != tenantID {
			return nil, ErrNotFound
		}
		return u, nil
	}
	// Bind the caller's tenant so the row + parts are only visible if
	// they belong to tenantID. A cross-tenant upload_id returns zero
	// rows -> ErrNotFound (404 NoSuchUpload), with no 403 existence
	// oracle.
	tx, err := rlsdb.BeginTenant(context.Background(), s.db, tenantID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	upload, err := s.loadUpload(tx, uploadID)
	if err != nil {
		return nil, err
	}
	// Application-layer tenant predicate (layer 1), redundant with the
	// RLS binding above (layer 2): loadUpload selects on upload_id alone,
	// so on a superuser / unarmed dev database — where Postgres bypasses
	// RLS — this is what keeps a cross-tenant upload_id fail-closed
	// (ErrNotFound, no 403 oracle). Under a non-superuser production role
	// RLS already returned zero rows, so this never fires there.
	if upload.TenantID != tenantID {
		return nil, ErrNotFound
	}
	if err := s.loadParts(tx, uploadID, upload); err != nil {
		return nil, err
	}
	s.sessions.Store(uploadID, upload)
	return upload, nil
}

// loadUpload reads the single upload row, unmarshals the policy,
// and (when wrapped_dek is populated) unwraps the DEK back into
// DEKMaterial. Returns ErrNotFound when the row is missing.
func (s *PostgresStore) loadUpload(q rowQuerier, uploadID string) (*Upload, error) {
	query := fmt.Sprintf(`
		SELECT tenant_id, bucket, object_key, version_id, backend, policy,
		       enc_mode, wrapped_dek, wrapped_key_id, wrap_algorithm,
		       content_algorithm, created_at
		FROM %s WHERE upload_id = $1
	`, s.uploadsTable)
	row := q.QueryRowContext(context.Background(), query, uploadID)
	var (
		policyJSON       []byte
		wrappedDEK       []byte
		wrappedKeyID     sql.NullString
		wrapAlg          sql.NullString
		contentAlg       sql.NullString
		encMode          sql.NullString
		backend          sql.NullString
		versionID        sql.NullString
		upload           = &Upload{ID: uploadID}
	)
	switch err := row.Scan(
		&upload.TenantID,
		&upload.Bucket,
		&upload.ObjectKey,
		&versionID,
		&backend,
		&policyJSON,
		&encMode,
		&wrappedDEK,
		&wrappedKeyID,
		&wrapAlg,
		&contentAlg,
		&upload.CreatedAt,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("multipart: scan upload: %w", err)
	}
	if err := json.Unmarshal(policyJSON, &upload.Policy); err != nil {
		return nil, fmt.Errorf("multipart: unmarshal policy: %w", err)
	}
	if versionID.Valid {
		upload.VersionID = versionID.String
	}
	if backend.Valid {
		upload.Backend = backend.String
	}
	if encMode.Valid {
		upload.EncMode = encMode.String
	}
	if wrappedKeyID.Valid {
		upload.WrappedKeyID = wrappedKeyID.String
	}
	if wrapAlg.Valid {
		upload.WrapAlgorithm = wrapAlg.String
	}
	if contentAlg.Valid {
		upload.ContentAlgorithm = contentAlg.String
	}
	upload.WrappedDEK = wrappedDEK

	if len(wrappedDEK) > 0 {
		if s.wrapper == nil {
			return nil, errors.New("multipart: upload was sealed with gateway encryption but PostgresConfig.Wrapper is nil")
		}
		dek, uerr := s.wrapper.UnwrapDEK(client_sdk.WrappedDEK{
			KeyID:         upload.WrappedKeyID,
			Algorithm:     upload.ContentAlgorithm,
			WrappedKey:    wrappedDEK,
			WrapAlgorithm: upload.WrapAlgorithm,
		}, s.cmk)
		if uerr != nil {
			return nil, fmt.Errorf("multipart: unwrap dek: %w", uerr)
		}
		upload.DEKMaterial = []byte(dek)
	}
	upload.parts = map[int]Part{}
	upload.PartHashes = make(map[int][]byte)
	upload.PlaintextPartHashes = make(map[int][]byte)
	return upload, nil
}

// loadParts populates upload.parts and the hash maps from the
// multipart_parts table.
func (s *PostgresStore) loadParts(q rowQuerier, uploadID string, upload *Upload) error {
	query := fmt.Sprintf(`
		SELECT part_number, piece_id, backend, etag, size_bytes,
		       part_hash, plaintext_part_hash, uploaded_at
		FROM %s WHERE upload_id = $1 ORDER BY part_number
	`, s.partsTable)
	rows, err := q.QueryContext(context.Background(), query, uploadID)
	if err != nil {
		return fmt.Errorf("multipart: query parts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			p             Part
			etag          sql.NullString
			sizeBytes     sql.NullInt64
			partHash      []byte
			plaintextHash []byte
		)
		if err := rows.Scan(&p.PartNumber, &p.PieceID, &p.Backend,
			&etag, &sizeBytes, &partHash, &plaintextHash, &p.UploadedAt); err != nil {
			return fmt.Errorf("multipart: scan part: %w", err)
		}
		if etag.Valid {
			p.ETag = etag.String
		}
		if sizeBytes.Valid {
			p.SizeBytes = sizeBytes.Int64
		}
		upload.parts[p.PartNumber] = p
		if len(partHash) > 0 {
			cp := make([]byte, len(partHash))
			copy(cp, partHash)
			upload.PartHashes[p.PartNumber] = cp
		}
		if len(plaintextHash) > 0 {
			cp := make([]byte, len(plaintextHash))
			copy(cp, plaintextHash)
			upload.PlaintextPartHashes[p.PartNumber] = cp
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("multipart: iterate parts: %w", err)
	}
	return nil
}

// PutPart upserts the part row. It also flushes the upload's
// current PartHash / PlaintextPartHash for partNumber so the row
// reflects whatever the handler captured on the in-memory Upload
// before this call.
func (s *PostgresStore) PutPart(tenantID, uploadID string, part Part) error {
	if part.PartNumber <= 0 {
		return errors.New("multipart: part_number must be positive")
	}
	upload, err := s.Get(tenantID, uploadID)
	if err != nil {
		return err
	}
	var (
		partHash      []byte
		plaintextHash []byte
	)
	if h, ok := upload.PartHash(part.PartNumber); ok {
		partHash = h
	}
	if h, ok := upload.PlaintextPartHash(part.PartNumber); ok {
		plaintextHash = h
	}
	q := fmt.Sprintf(`
		INSERT INTO %s (
			upload_id, tenant_id, part_number, piece_id, backend,
			etag, size_bytes, part_hash, plaintext_part_hash, uploaded_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (upload_id, part_number) DO UPDATE SET
			-- Self-heal the denormalised tenant_id on re-upload: in steady
			-- state it already equals EXCLUDED.tenant_id (no-op), but if a
			-- pre-migration row predates the backfill its NULL gets fixed
			-- here rather than lingering invisible under RLS.
			tenant_id           = EXCLUDED.tenant_id,
			piece_id            = EXCLUDED.piece_id,
			backend             = EXCLUDED.backend,
			etag                = EXCLUDED.etag,
			size_bytes          = EXCLUDED.size_bytes,
			part_hash           = EXCLUDED.part_hash,
			plaintext_part_hash = EXCLUDED.plaintext_part_hash,
			uploaded_at         = EXCLUDED.uploaded_at
	`, s.partsTable)
	uploadedAt := part.UploadedAt
	if uploadedAt.IsZero() {
		uploadedAt = s.clock()
	}
	// Bind the tenant so the parts RLS WITH CHECK admits the row. The
	// part's tenant_id is the caller's tenant — identical to the owning
	// upload's tenant, which Get above has already confirmed.
	tx, err := rlsdb.BeginTenant(context.Background(), s.db, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(context.Background(), q,
		uploadID,
		tenantID,
		part.PartNumber,
		part.PieceID,
		part.Backend,
		nullableString(part.ETag),
		nullableInt64(part.SizeBytes),
		nullableBytes(partHash),
		nullableBytes(plaintextHash),
		uploadedAt,
	); err != nil {
		return fmt.Errorf("multipart: upsert part: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("multipart: commit put part: %w", err)
	}
	upload.mu.Lock()
	upload.parts[part.PartNumber] = Part{
		PartNumber: part.PartNumber,
		PieceID:    part.PieceID,
		Backend:    part.Backend,
		ETag:       part.ETag,
		SizeBytes:  part.SizeBytes,
		UploadedAt: uploadedAt,
	}
	upload.mu.Unlock()
	return nil
}

// Complete finalises the upload. It validates that tenantID,
// bucket, and objectKey match the recorded upload, that every
// PartReference names a known part with a matching ETag, and then
// deletes the upload row (cascading the parts) within a single
// transaction so a Complete that races an Abort cannot leak a
// half-deleted record.
func (s *PostgresStore) Complete(uploadID, tenantID, bucket, objectKey string, expected []PartReference) ([]Part, *Upload, error) {
	// Scoped to tenantID: a cross-tenant upload_id is invisible under
	// RLS, so Get returns ErrNotFound (404) rather than a 403 oracle,
	// and the upload is left intact for its real owner.
	upload, err := s.Get(tenantID, uploadID)
	if err != nil {
		return nil, nil, err
	}
	if upload.Bucket != bucket || upload.ObjectKey != objectKey {
		return nil, nil, ErrUploadMismatch
	}
	upload.mu.Lock()
	result := make([]Part, 0, len(expected))
	for _, ref := range expected {
		part, ok := upload.parts[ref.PartNumber]
		if !ok {
			upload.mu.Unlock()
			return nil, nil, ErrPartNotFound
		}
		if ref.ETag != "" && part.ETag != "" && stripQuotes(ref.ETag) != stripQuotes(part.ETag) {
			upload.mu.Unlock()
			return nil, nil, ErrPartETagMismatch
		}
		result = append(result, part)
	}
	upload.mu.Unlock()
	sort.Slice(result, func(i, j int) bool { return result[i].PartNumber < result[j].PartNumber })

	// Tenant-bound DELETE: under RLS an unscoped DELETE would match zero
	// rows (fail-closed), silently leaving the upload behind. Bind the
	// tenant so the row is visible; the parts cascade with it.
	if err := s.deleteUpload(tenantID, uploadID); err != nil {
		return nil, nil, err
	}
	s.sessions.Delete(uploadID)
	return result, upload, nil
}

// Abort removes the upload row (cascading parts) and returns the
// parts list so the caller can DeletePiece each one.
func (s *PostgresStore) Abort(uploadID, tenantID string) (*Upload, []Part, error) {
	// Scoped to tenantID like Complete: a cross-tenant upload_id is
	// invisible under RLS, so Get returns ErrNotFound (404) and the
	// upload is left intact for its real owner.
	upload, err := s.Get(tenantID, uploadID)
	if err != nil {
		return nil, nil, err
	}
	upload.mu.Lock()
	parts := make([]Part, 0, len(upload.parts))
	for _, p := range upload.parts {
		parts = append(parts, p)
	}
	upload.mu.Unlock()
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })

	if err := s.deleteUpload(tenantID, uploadID); err != nil {
		return nil, nil, err
	}
	s.sessions.Delete(uploadID)
	return upload, parts, nil
}

// deleteUpload removes the upload row (cascading its parts) inside a
// transaction bound to tenantID, so RLS admits the row. Shared by
// Complete and Abort.
func (s *PostgresStore) deleteUpload(tenantID, uploadID string) error {
	tx, err := rlsdb.BeginTenant(context.Background(), s.db, tenantID)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	q := fmt.Sprintf(`DELETE FROM %s WHERE upload_id = $1`, s.uploadsTable)
	if _, err := tx.ExecContext(context.Background(), q, uploadID); err != nil {
		return fmt.Errorf("multipart: delete upload: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("multipart: commit delete upload: %w", err)
	}
	return nil
}

// List returns all uploads scoped to (tenantID, bucket). Loads
// every matching row fresh from Postgres so multi-node fleets see
// each other's in-flight uploads. Parts are NOT loaded — the
// administrative ListMultipartUploads endpoint only needs the
// upload metadata.
func (s *PostgresStore) List(tenantID, bucket string) []*Upload {
	q := fmt.Sprintf(`
		SELECT upload_id, tenant_id, bucket, object_key, backend,
		       policy, enc_mode, wrapped_key_id, wrap_algorithm,
		       content_algorithm, created_at
		FROM %s
		WHERE tenant_id = $1
		  AND ($2 = '' OR bucket = $2)
		ORDER BY created_at
	`, s.uploadsTable)
	// Bind the tenant so RLS scopes the listing even though the WHERE
	// clause already filters on tenant_id (defence-in-depth, matching
	// the other tenant-scoped stores).
	tx, err := rlsdb.BeginTenant(context.Background(), s.db, tenantID)
	if err != nil {
		s.logf("list uploads begin: %v", err)
		return nil
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(context.Background(), q, tenantID, bucket)
	if err != nil {
		s.logf("list uploads: %v", err)
		return nil
	}
	defer rows.Close()
	var out []*Upload
	for rows.Next() {
		var (
			id, tID, b, key string
			backend         sql.NullString
			policyJSON      []byte
			encMode         sql.NullString
			wrappedKeyID    sql.NullString
			wrapAlg         sql.NullString
			contentAlg      sql.NullString
			createdAt       time.Time
		)
		if err := rows.Scan(&id, &tID, &b, &key, &backend, &policyJSON,
			&encMode, &wrappedKeyID, &wrapAlg, &contentAlg, &createdAt); err != nil {
			s.logf("scan upload row: %v", err)
			continue
		}
		u := &Upload{
			ID:        id,
			TenantID:  tID,
			Bucket:    b,
			ObjectKey: key,
			CreatedAt: createdAt,
		}
		if backend.Valid {
			u.Backend = backend.String
		}
		if encMode.Valid {
			u.EncMode = encMode.String
		}
		if wrappedKeyID.Valid {
			u.WrappedKeyID = wrappedKeyID.String
		}
		if wrapAlg.Valid {
			u.WrapAlgorithm = wrapAlg.String
		}
		if contentAlg.Valid {
			u.ContentAlgorithm = contentAlg.String
		}
		if len(policyJSON) > 0 {
			_ = json.Unmarshal(policyJSON, &u.Policy)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		s.logf("iterate list: %v", err)
	}
	return out
}

// runExpirySweeper is the background goroutine that deletes
// uploads older than UploadTTL. It runs at ExpirySweepInterval
// cadence until Close is called.
func (s *PostgresStore) runExpirySweeper() {
	defer close(s.sweepDone)
	ticker := time.NewTicker(s.expirySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopSweep:
			return
		case <-ticker.C:
			if err := s.sweepExpired(); err != nil {
				s.logf("expiry sweep: %v", err)
			}
		}
	}
}

// sweepExpired scans for uploads past UploadTTL, dispatches the
// cleanup callback for each, and deletes the rows. The cascade on
// multipart_parts handles the part records automatically.
func (s *PostgresStore) sweepExpired() error {
	cutoff := s.clock().Add(-s.uploadTTL)
	q := fmt.Sprintf(`
		SELECT upload_id, tenant_id FROM %s WHERE created_at < $1
		ORDER BY created_at
		LIMIT 1000
	`, s.uploadsTable)
	// The expiry enumeration crosses tenants by design, so it runs under
	// the audited scan_all bypass. expireOne then re-binds each upload's
	// own tenant for the load + delete, so no cross-tenant write is ever
	// performed (the policy's WITH CHECK does not honour scan_all anyway).
	enum, err := rlsdb.BeginScanAll(context.Background(), s.db)
	if err != nil {
		return fmt.Errorf("multipart: begin expiry scan: %w", err)
	}
	defer func() { _ = enum.Rollback() }()
	rows, err := enum.QueryContext(context.Background(), q, cutoff)
	if err != nil {
		return fmt.Errorf("multipart: query expired: %w", err)
	}
	type expired struct{ id, tenantID string }
	var victims []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.tenantID); err != nil {
			rows.Close()
			return fmt.Errorf("multipart: scan expired id: %w", err)
		}
		victims = append(victims, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("multipart: iterate expired: %w", err)
	}
	// Release the scan_all transaction before re-binding per-tenant.
	_ = enum.Rollback()
	for _, v := range victims {
		s.expireOne(v.tenantID, v.id)
	}
	return nil
}

// expireOne loads the upload + parts (so the cleanup callback can
// fan out to providers), invokes the callback, then deletes the
// upload row. Failures in cleanup are logged but do not block the
// row deletion — the alternative would be an infinitely retrying
// sweep on a single broken upload.
func (s *PostgresStore) expireOne(tenantID, uploadID string) {
	// Phase 1 — load the upload + parts under a short, read-only,
	// tenant-bound transaction and release it *before* the cleanup
	// callback. The callback fans out to storage backends with a 30s
	// timeout (network I/O); holding the load transaction open across it
	// would pin a pool connection idle for the whole callback, and with
	// up to 1000 victims swept sequentially a slow backend could starve
	// the pool. The delete runs in its own transaction below (Phase 2),
	// matching how Complete/Abort split Get from deleteUpload.
	upload, err := func() (*Upload, error) {
		tx, err := rlsdb.BeginTenant(context.Background(), s.db, tenantID)
		if err != nil {
			return nil, err
		}
		defer func() { _ = tx.Rollback() }()
		u, err := s.loadUpload(tx, uploadID)
		if err != nil {
			return nil, err
		}
		if err := s.loadParts(tx, uploadID, u); err != nil {
			return nil, err
		}
		return u, nil
	}()
	if err != nil {
		s.logf("expire load %s: %v", uploadID, err)
		return
	}
	// Defence-in-depth: mirror Get's app-layer tenant check so the sweep
	// stays fail-closed even on a superuser/unarmed dev DB where RLS is
	// bypassed. tenantID came from this row's own tenant_id in the sweep
	// scan, so a mismatch means the row was re-tenanted between scan and
	// load — skip it rather than delete (or run cleanup) under the wrong
	// tenant binding.
	if upload.TenantID != tenantID {
		s.logf("expire skip %s: tenant mismatch", uploadID)
		return
	}
	if s.cleanup != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		parts := make([]Part, 0, len(upload.parts))
		for _, p := range upload.parts {
			parts = append(parts, p)
		}
		sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
		func() {
			defer cancel()
			defer func() {
				if r := recover(); r != nil {
					s.logf("expire cleanup panic for %s: %v", uploadID, r)
				}
			}()
			s.cleanup(ctx, upload, parts)
		}()
	}
	// Phase 2 — delete the row (parts cascade) under its own short
	// tenant-bound transaction, so the pool connection is only held for
	// the DELETE, never across the cleanup callback above.
	if err := s.deleteUpload(tenantID, uploadID); err != nil {
		s.logf("expire delete %s: %v", uploadID, err)
		return
	}
	s.sessions.Delete(uploadID)
	s.logf("expired multipart upload %s (created %s)", uploadID, upload.CreatedAt.Format(time.RFC3339))
}

func (s *PostgresStore) logf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Printf("multipart_postgres: "+format, args...)
}

// nullableString returns sql.NullString{Valid: false} when s == "".
func nullableString(s string) any {
	if s == "" {
		return sql.NullString{}
	}
	return s
}

// nullableInt64 returns sql.NullInt64{Valid: false} when n == 0
// (the zero value the existing API treats as "unset").
func nullableInt64(n int64) any {
	if n == 0 {
		return sql.NullInt64{}
	}
	return n
}

// nullableBytes returns nil when b is empty so Postgres records a
// NULL rather than an empty bytea — this matters because LENGTH(NULL)
// is NULL but LENGTH('') is 0, and the cleanup query in Get treats
// any non-NULL value as "wrapped".
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// isSafeMultipartIdent mirrors metadata/manifest_store/postgres'
// identifier check: ASCII letters, digits, and underscore only, no
// leading digit. Kept private to this file so the multipart store
// has no compile-time dependency on the manifest_store package.
func isSafeMultipartIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		switch {
		case isLetter:
		case isDigit && i > 0:
		default:
			return false
		}
	}
	return true
}

// Compile-time interface check: PostgresStore must satisfy Store.
var _ Store = (*PostgresStore)(nil)
