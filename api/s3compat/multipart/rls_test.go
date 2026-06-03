package multipart

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"github.com/kennguy3n/zk-object-fabric/internal/rlsdb"
)

// Row-Level Security enforcement is a property of Postgres, not of Go,
// so it can only be verified against a live database — and only when the
// app connects as a non-superuser role (Postgres bypasses RLS for
// superusers and BYPASSRLS roles regardless of FORCE). These tests use
// two DSNs, mirroring the manifest / content_index / bucket_config RLS
// suites:
//
//	METADATA_DSN     — a privileged role used to create both multipart
//	                   tables, arm RLS, and seed a second tenant's rows.
//	METADATA_APP_DSN — the least-privilege, non-superuser role the
//	                   gateway connects as in production. The enforcement
//	                   assertions run over this connection.
//
// Both are skipped when unset so the unit suite stays hermetic. Unlike
// the other stores, the multipart store spans *two* tenant-scoped tables
// (multipart_uploads and multipart_parts), so this suite arms and
// exercises the WITH CHECK rejection on each.

const (
	rlsUploadsTable = "multipart_uploads_rls_test"
	rlsPartsTable   = "multipart_parts_rls_test"
	rlsAppRole      = "zkof_rls_test"
)

func openRLSPQ(t *testing.T, dsn string) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	return db
}

// armMultipartRLS (re)creates both multipart tables owned by the
// privileged role and arms Row-Level Security on each via the shared
// production DDL helper (rlsdb.Statements), exactly as an operator would.
func armMultipartRLS(t *testing.T, priv *sql.DB) {
	t.Helper()
	exec := func(q string) {
		if _, err := priv.Exec(q); err != nil {
			t.Fatalf("setup %q: %v", q, err)
		}
	}
	exec(`DROP TABLE IF EXISTS ` + rlsPartsTable)
	exec(`DROP TABLE IF EXISTS ` + rlsUploadsTable)
	exec(`CREATE TABLE ` + rlsUploadsTable + ` (
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
		created_at        TIMESTAMPTZ NOT NULL DEFAULT now()
	)`)
	exec(`CREATE TABLE ` + rlsPartsTable + ` (
		upload_id           TEXT NOT NULL REFERENCES ` + rlsUploadsTable + `(upload_id) ON DELETE CASCADE,
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
	)`)
	for _, table := range []string{rlsUploadsTable, rlsPartsTable} {
		stmts, err := rlsdb.Statements(table, rlsAppRole)
		if err != nil {
			t.Fatalf("rlsdb.Statements(%s): %v", table, err)
		}
		for _, s := range stmts {
			if _, err := priv.Exec(s); err != nil {
				t.Fatalf("arm RLS %q: %v", s, err)
			}
		}
	}
	t.Cleanup(func() {
		_, _ = priv.Exec(`DROP TABLE IF EXISTS ` + rlsPartsTable)
		_, _ = priv.Exec(`DROP TABLE IF EXISTS ` + rlsUploadsTable)
	})
}

func rlsUpload(id, tenant string, createdAt time.Time) *Upload {
	return &Upload{
		ID:        id,
		TenantID:  tenant,
		Bucket:    "bkt",
		ObjectKey: "obj-" + id,
		Backend:   "be0",
		CreatedAt: createdAt,
	}
}

func newRLSStore(t *testing.T, db *sql.DB) *PostgresStore {
	t.Helper()
	store, err := NewPostgresStore(PostgresConfig{
		DB:                  db,
		UploadsTable:        rlsUploadsTable,
		PartsTable:          rlsPartsTable,
		UploadTTL:           1 * time.Hour,
		ExpirySweepInterval: 1 * time.Hour, // driven manually
	})
	if err != nil {
		t.Fatalf("NewPostgresStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRLS_Multipart_CrossTenantIsolation(t *testing.T) {
	privDSN := os.Getenv("METADATA_DSN")
	if privDSN == "" {
		t.Skip("METADATA_DSN not set; skipping live RLS tests")
	}
	appDSN := os.Getenv("METADATA_APP_DSN")
	if appDSN == "" {
		t.Skip("METADATA_APP_DSN (non-superuser role) not set; skipping RLS enforcement tests")
	}
	ctx := context.Background()

	priv := openRLSPQ(t, privDSN)
	defer priv.Close()
	armMultipartRLS(t, priv)

	app := openRLSPQ(t, appDSN)
	defer app.Close()

	// Refuse to run the enforcement assertions if the app DSN is secretly
	// privileged — RLS would be bypassed and every assertion would pass
	// vacuously.
	var superuser string
	if err := app.QueryRow(`SELECT current_setting('is_superuser')`).Scan(&superuser); err != nil {
		t.Fatalf("probe is_superuser: %v", err)
	}
	if superuser != "off" {
		t.Fatalf("METADATA_APP_DSN role is a superuser (is_superuser=%s); RLS would be bypassed", superuser)
	}

	appStore := newRLSStore(t, app)
	privStore := newRLSStore(t, priv)

	// t1 creates its own upload + part through the non-superuser app store
	// (BeginTenant binds zkof.tenant_id=t1, so WITH CHECK admits the rows).
	if err := appStore.Create(rlsUpload("u-t1", "t1", time.Now())); err != nil {
		t.Fatalf("app Create u-t1: %v", err)
	}
	if err := appStore.PutPart("t1", "u-t1", Part{PartNumber: 1, PieceID: "p1", Backend: "be0", ETag: "e1"}); err != nil {
		t.Fatalf("app PutPart u-t1: %v", err)
	}
	// t2's upload + part are seeded via the privileged store (a real second
	// tenant). The privileged store binds t2 in its own BeginTenant.
	if err := privStore.Create(rlsUpload("u-t2", "t2", time.Now())); err != nil {
		t.Fatalf("priv Create u-t2: %v", err)
	}
	if err := privStore.PutPart("t2", "u-t2", Part{PartNumber: 1, PieceID: "p2", Backend: "be0", ETag: "e2"}); err != nil {
		t.Fatalf("priv PutPart u-t2: %v", err)
	}

	t.Run("unscoped raw query is fail-closed on both tables", func(t *testing.T) {
		// No GUC bound at all → zero visible rows under FORCE'd RLS.
		for _, table := range []string{rlsUploadsTable, rlsPartsTable} {
			var n int
			if err := app.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				t.Fatalf("count (no GUC) %s: %v", table, err)
			}
			if n != 0 {
				t.Fatalf("no-GUC count on %s = %d, want 0 (RLS must hide every row when unscoped)", table, n)
			}
		}
	})

	t.Run("app store reads its own tenant and misses cross-tenant", func(t *testing.T) {
		got, err := appStore.Get("t1", "u-t1")
		if err != nil {
			t.Fatalf("Get t1/u-t1: %v", err)
		}
		if got.TenantID != "t1" || got.ObjectKey != "obj-u-t1" {
			t.Fatalf("Get t1/u-t1 = %+v, want tenant t1 obj-u-t1", got)
		}
		if len(got.Parts()) != 1 || got.Parts()[1].PieceID != "p1" {
			t.Fatalf("Get t1/u-t1 parts = %+v, want one part p1", got.Parts())
		}
		// t1 must not be able to see (or even confirm the existence of)
		// t2's upload: cross-tenant id → ErrNotFound, no 403 oracle.
		if _, err := appStore.Get("t1", "u-t2"); err != ErrNotFound {
			t.Fatalf("cross-tenant Get t1/u-t2 err = %v, want ErrNotFound", err)
		}
	})

	t.Run("cross-tenant write is rejected by WITH CHECK on multipart_uploads", func(t *testing.T) {
		tx, err := rlsdb.BeginTenant(ctx, app, "t1")
		if err != nil {
			t.Fatalf("BeginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+rlsUploadsTable+` (upload_id, tenant_id, bucket, object_key, policy)
			 VALUES ('u-evil', 't2', 'b', 'k', '{}'::jsonb)`)
		if err == nil {
			t.Fatal("cross-tenant upload INSERT under t1 scope succeeded; WITH CHECK should reject it")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("cross-tenant upload INSERT error = %v, want a row-level security violation", err)
		}
	})

	t.Run("cross-tenant write is rejected by WITH CHECK on multipart_parts", func(t *testing.T) {
		// Bind t1 and try to attach a part to t2's upload tagged as t2.
		// The policy's WITH CHECK keys on the part's own tenant_id, so it
		// must reject a tenant_id that differs from the bound GUC.
		tx, err := rlsdb.BeginTenant(ctx, app, "t1")
		if err != nil {
			t.Fatalf("BeginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+rlsPartsTable+` (upload_id, tenant_id, part_number, piece_id, backend)
			 VALUES ('u-t2', 't2', 2, 'p-evil', 'be0')`)
		if err == nil {
			t.Fatal("cross-tenant part INSERT under t1 scope succeeded; WITH CHECK should reject it")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("cross-tenant part INSERT error = %v, want a row-level security violation", err)
		}
	})

	t.Run("scan_all sweeper expires every tenant's uploads", func(t *testing.T) {
		// Age both uploads past a 1ms TTL and drive the sweep on the
		// non-superuser app store. sweepExpired enumerates across tenants
		// under the audited scan_all bypass, then re-binds each upload's
		// own tenant to delete it + cascade its parts.
		old := time.Now().Add(-time.Hour)
		if _, err := priv.ExecContext(ctx,
			`UPDATE `+rlsUploadsTable+` SET created_at = $1`, old); err != nil {
			t.Fatalf("age uploads: %v", err)
		}
		appStore.uploadTTL = 1 * time.Millisecond
		// Drop the app store's session cache so the post-sweep Get below
		// actually hits Postgres rather than a stale cached pointer.
		appStore.sessions.Range(func(k, _ any) bool { appStore.sessions.Delete(k); return true })

		if err := appStore.sweepExpired(); err != nil {
			t.Fatalf("sweepExpired: %v", err)
		}

		// Both uploads (and their parts) are gone — verified with the
		// privileged store so no RLS scoping hides a survivor.
		var uploads, parts int
		if err := priv.QueryRowContext(ctx, `SELECT count(*) FROM `+rlsUploadsTable).Scan(&uploads); err != nil {
			t.Fatalf("count uploads post-sweep: %v", err)
		}
		if err := priv.QueryRowContext(ctx, `SELECT count(*) FROM `+rlsPartsTable).Scan(&parts); err != nil {
			t.Fatalf("count parts post-sweep: %v", err)
		}
		if uploads != 0 || parts != 0 {
			t.Fatalf("post-sweep counts uploads=%d parts=%d, want 0/0 (scan_all sweep must expire every tenant)", uploads, parts)
		}
	})
}
