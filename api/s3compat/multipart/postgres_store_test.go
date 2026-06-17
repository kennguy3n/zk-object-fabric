package multipart

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestIsSafeMultipartIdent(t *testing.T) {
	for _, ok := range []string{"multipart_uploads", "multipart_parts", "abc", "abc_DEF_123"} {
		if !isSafeMultipartIdent(ok) {
			t.Errorf("isSafeMultipartIdent(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "1leadingdigit", "with space", "drop;table", "dash-name"} {
		if isSafeMultipartIdent(bad) {
			t.Errorf("isSafeMultipartIdent(%q) = true, want false", bad)
		}
	}
}

func TestNullableHelpers(t *testing.T) {
	if got := nullableString(""); got != (sql.NullString{}) {
		t.Errorf("nullableString(%q) = %v, want zero NullString", "", got)
	}
	if got := nullableString("hello"); got != "hello" {
		t.Errorf("nullableString(%q) = %v, want %q", "hello", got, "hello")
	}
	if got := nullableInt64(0); got != (sql.NullInt64{}) {
		t.Errorf("nullableInt64(0) = %v, want zero NullInt64", got)
	}
	if got := nullableInt64(42); got != int64(42) {
		t.Errorf("nullableInt64(42) = %v, want 42", got)
	}
	if got := nullableBytes(nil); got != nil {
		t.Errorf("nullableBytes(nil) = %v, want nil", got)
	}
	if got := nullableBytes([]byte{1, 2, 3}); got == nil {
		t.Errorf("nullableBytes([1,2,3]) = nil, want non-nil")
	}
}

// requireMultipartPostgres returns an open *sql.DB with the
// multipart schema applied, or skips the test when METADATA_DSN
// is not configured. The schema is dropped on cleanup so leftover
// rows from a previous run do not interfere with subsequent runs.
func requireMultipartPostgres(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping postgres multipart tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS multipart_parts_test`); err != nil {
		t.Fatalf("drop parts: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS multipart_uploads_test`); err != nil {
		t.Fatalf("drop uploads: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE multipart_uploads_test (
			upload_id         TEXT PRIMARY KEY,
			tenant_id         TEXT NOT NULL,
			bucket            TEXT NOT NULL,
			object_key        TEXT NOT NULL,
			version_id        TEXT,
			backend           TEXT,
			policy            JSONB NOT NULL,
			enc_mode          TEXT,
			wrapped_dek       BYTEA,
			wrapped_key_id    TEXT,
			wrap_algorithm    TEXT,
			content_algorithm TEXT,
			metadata          JSONB,
			created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		t.Fatalf("create uploads: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE multipart_parts_test (
			upload_id           TEXT NOT NULL REFERENCES multipart_uploads_test(upload_id) ON DELETE CASCADE,
			tenant_id           TEXT,
			part_number         INTEGER NOT NULL,
			piece_id            TEXT NOT NULL,
			backend             TEXT NOT NULL,
			etag                TEXT,
			size_bytes          BIGINT,
			part_hash           BYTEA,
			plaintext_part_hash BYTEA,
			uploaded_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (upload_id, part_number)
		)
	`); err != nil {
		t.Fatalf("create parts: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS multipart_parts_test`)
		_, _ = db.ExecContext(context.Background(), `DROP TABLE IF EXISTS multipart_uploads_test`)
		_ = db.Close()
	})
	return db
}

// TestPostgresStore_CreateGetCompleteAbort covers the
// create→put-part→complete and create→put-part→abort lifecycles.
// Gated on METADATA_DSN so the test suite can run without
// Postgres locally.
func TestPostgresStore_CreateGetCompleteAbort(t *testing.T) {
	db := requireMultipartPostgres(t)
	store, err := NewPostgresStore(PostgresConfig{
		DB:                  db,
		UploadsTable:        "multipart_uploads_test",
		PartsTable:          "multipart_parts_test",
		UploadTTL:           1 * time.Hour,
		ExpirySweepInterval: 1 * time.Hour, // long enough to not interfere
	})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	upload := &Upload{
		ID:        "u1",
		TenantID:  "tenant-a",
		Bucket:    "bkt",
		ObjectKey: "obj",
		Backend:   "be0",
		CreatedAt: time.Now(),
	}
	if err := store.Create(upload); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Round-trip Get.
	got, err := store.Get("tenant-a", "u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TenantID != "tenant-a" || got.Bucket != "bkt" || got.ObjectKey != "obj" {
		t.Errorf("Get returned wrong fields: %+v", got)
	}

	// PutPart with a hash captured on the upload pointer (matches
	// the multipart_handler flow).
	got.SetPartHash(1, []byte("hash-1"))
	if err := store.PutPart("tenant-a", "u1", Part{
		PartNumber: 1, PieceID: "p1", Backend: "be0", ETag: "etag1", SizeBytes: 10,
	}); err != nil {
		t.Fatalf("PutPart: %v", err)
	}

	// Complete with a matching reference succeeds.
	parts, returned, err := store.Complete("u1", "tenant-a", "bkt", "obj", []PartReference{{PartNumber: 1, ETag: "etag1"}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(parts) != 1 || parts[0].PieceID != "p1" {
		t.Errorf("Complete parts = %+v, want one part with piece p1", parts)
	}
	if returned.ID != "u1" {
		t.Errorf("Complete returned upload ID %q, want u1", returned.ID)
	}

	// After Complete the upload is gone.
	if _, err := store.Get("tenant-a", "u1"); err != ErrNotFound {
		t.Errorf("post-complete Get err = %v, want ErrNotFound", err)
	}

	// Abort lifecycle.
	upload2 := &Upload{ID: "u2", TenantID: "tenant-a", Bucket: "bkt", ObjectKey: "obj2"}
	if err := store.Create(upload2); err != nil {
		t.Fatalf("Create u2: %v", err)
	}
	if _, _, err := store.Abort("u2", "tenant-a"); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, err := store.Get("tenant-a", "u2"); err != ErrNotFound {
		t.Errorf("post-abort Get err = %v, want ErrNotFound", err)
	}
}

// TestPostgresStore_MetadataDurableRoundTrip verifies that the tags +
// object metadata captured at CreateMultipartUpload survive a reload
// from Postgres on a different node. A second store instance (its own
// empty session cache) forces Get to hit loadUpload rather than return
// the create-time pointer, mirroring a Complete served by another node.
func TestPostgresStore_MetadataDurableRoundTrip(t *testing.T) {
	db := requireMultipartPostgres(t)
	newStore := func() *PostgresStore {
		s, err := NewPostgresStore(PostgresConfig{
			DB:                  db,
			UploadsTable:        "multipart_uploads_test",
			PartsTable:          "multipart_parts_test",
			ExpirySweepInterval: 1 * time.Hour,
		})
		if err != nil {
			t.Fatalf("NewPostgresStore: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	createStore := newStore()
	md := ObjectMetadata{
		Tags:               map[string]string{"team": "drive", "env": "prod"},
		ContentType:        "image/png",
		ContentDisposition: `attachment; filename="x.png"`,
		CacheControl:       "max-age=3600",
		UserMetadata:       map[string]string{"author": "ken"},
	}
	if err := createStore.Create(&Upload{
		ID: "u-md", TenantID: "tenant-a", Bucket: "bkt", ObjectKey: "obj", Metadata: md,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Reload through a fresh store so the row is read back from Postgres.
	got, err := newStore().Get("tenant-a", "u-md")
	if err != nil {
		t.Fatalf("Get from fresh store: %v", err)
	}
	if got.Metadata.ContentType != "image/png" ||
		got.Metadata.ContentDisposition != `attachment; filename="x.png"` ||
		got.Metadata.CacheControl != "max-age=3600" {
		t.Errorf("system metadata round-trip mismatch: %+v", got.Metadata)
	}
	if got.Metadata.Tags["team"] != "drive" || got.Metadata.Tags["env"] != "prod" {
		t.Errorf("tags round-trip mismatch: %+v", got.Metadata.Tags)
	}
	if got.Metadata.UserMetadata["author"] != "ken" {
		t.Errorf("user metadata round-trip mismatch: %+v", got.Metadata.UserMetadata)
	}

	// An upload created without metadata reads back zero-valued (NULL
	// column), not an error.
	if err := createStore.Create(&Upload{
		ID: "u-nomd", TenantID: "tenant-a", Bucket: "bkt", ObjectKey: "obj2",
	}); err != nil {
		t.Fatalf("Create no-metadata: %v", err)
	}
	plain, err := newStore().Get("tenant-a", "u-nomd")
	if err != nil {
		t.Fatalf("Get no-metadata: %v", err)
	}
	if !plain.Metadata.IsZero() {
		t.Errorf("expected zero metadata, got %+v", plain.Metadata)
	}
}

// TestPostgresStore_CrossTenantAbortIsNotFound verifies that Abort
// against the wrong tenant is reported as ErrNotFound (the foreign
// upload is invisible under the caller's tenant binding — no 403
// existence oracle) and leaves the upload intact for its real owner.
func TestPostgresStore_CrossTenantAbortIsNotFound(t *testing.T) {
	db := requireMultipartPostgres(t)
	store, err := NewPostgresStore(PostgresConfig{
		DB:                  db,
		UploadsTable:        "multipart_uploads_test",
		PartsTable:          "multipart_parts_test",
		ExpirySweepInterval: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Create(&Upload{ID: "u3", TenantID: "tenant-a", Bucket: "b", ObjectKey: "k"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, _, err := store.Abort("u3", "wrong-tenant"); err != ErrNotFound {
		t.Errorf("Abort wrong tenant err = %v, want ErrNotFound", err)
	}
	// The real owner can still see and abort it.
	if _, err := store.Get("tenant-a", "u3"); err != nil {
		t.Errorf("Get after wrong-tenant Abort err = %v, want nil", err)
	}
}

// TestPostgresStore_ExpirySweeper verifies that sweepExpired
// deletes uploads older than UploadTTL and fires the Cleanup
// callback. The expiry sweep is invoked directly so the test does
// not depend on the goroutine's sleep cadence.
func TestPostgresStore_ExpirySweeper(t *testing.T) {
	db := requireMultipartPostgres(t)
	var sweeperCalls int
	store, err := NewPostgresStore(PostgresConfig{
		DB:                  db,
		UploadsTable:        "multipart_uploads_test",
		PartsTable:          "multipart_parts_test",
		UploadTTL:           1 * time.Millisecond,
		ExpirySweepInterval: 1 * time.Hour, // we drive sweep manually
		Cleanup: func(ctx context.Context, _ *Upload, _ []Part) {
			sweeperCalls++
		},
	})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	old := time.Now().Add(-1 * time.Hour)
	if err := store.Create(&Upload{ID: "u-old", TenantID: "t", Bucket: "b", ObjectKey: "k", CreatedAt: old}); err != nil {
		t.Fatalf("Create old upload: %v", err)
	}
	if err := store.sweepExpired(); err != nil {
		t.Fatalf("sweepExpired: %v", err)
	}
	if sweeperCalls != 1 {
		t.Errorf("cleanup callback fired %d times, want 1", sweeperCalls)
	}
	if _, err := store.Get("t", "u-old"); err != ErrNotFound {
		t.Errorf("post-sweep Get err = %v, want ErrNotFound", err)
	}
}
