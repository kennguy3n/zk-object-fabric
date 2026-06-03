package postgres

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/internal/rlsdb"
)

// RLS enforcement is a property of Postgres, not of Go, so it can only be
// verified against a live database — and only when the app connects as a
// non-superuser role (Postgres bypasses RLS for superusers and BYPASSRLS
// roles regardless of FORCE). These tests therefore use two DSNs:
//
//	METADATA_DSN     — a privileged role used to create the table, arm
//	                   RLS, and seed a second tenant's row.
//	METADATA_APP_DSN — the least-privilege, non-superuser role the
//	                   gateway connects as in production. The enforcement
//	                   assertions run over this connection.
//
// Both are skipped when unset so the unit suite stays hermetic.

const (
	ciRLSTestTable = "content_index_rls_test"
	ciRLSTestRole  = "zkof_rls_test"
)

func openPQ(t *testing.T, dsn string) *sql.DB {
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

// armRLSTestTable (re)creates the test table owned by the privileged role
// and arms Row-Level Security on it via the production DDL helper.
func armRLSTestTable(t *testing.T, priv *sql.DB) {
	t.Helper()
	if _, err := priv.Exec(`DROP TABLE IF EXISTS ` + ciRLSTestTable); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := priv.Exec(`
		CREATE TABLE ` + ciRLSTestTable + ` (
			tenant_id      TEXT        NOT NULL,
			content_hash   TEXT        NOT NULL,
			piece_id       TEXT        NOT NULL,
			backend        TEXT        NOT NULL,
			ref_count      INT         NOT NULL DEFAULT 1 CHECK (ref_count >= 0),
			size_bytes     BIGINT      NOT NULL DEFAULT 0,
			etag           TEXT        NULL,
			piece_ids      JSONB       NULL,
			plaintext_hash TEXT        NULL,
			created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, content_hash)
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	stmts, err := RLSStatements(ciRLSTestTable, ciRLSTestRole)
	if err != nil {
		t.Fatalf("RLSStatements: %v", err)
	}
	for _, s := range stmts {
		if _, err := priv.Exec(s); err != nil {
			t.Fatalf("arm RLS stmt %q: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = priv.Exec(`DROP TABLE IF EXISTS ` + ciRLSTestTable) })
}

func entryFor(tenant string) content_index.ContentIndexEntry {
	return content_index.ContentIndexEntry{
		TenantID:    tenant,
		ContentHash: "h-" + tenant,
		PieceID:     "p-" + tenant,
		Backend:     "mem",
	}
}

func TestRLSStatements_Alias(t *testing.T) {
	// The package-local alias must delegate to rlsdb.Statements and carry
	// the content_index table name through.
	stmts, err := RLSStatements("content_index", "zkof_app")
	if err != nil {
		t.Fatalf("RLSStatements: %v", err)
	}
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "CREATE POLICY tenant_isolation ON content_index") {
		t.Errorf("alias output missing content_index policy:\n%s", joined)
	}
}

func TestRLS_CrossTenantIsolation(t *testing.T) {
	privDSN := os.Getenv("METADATA_DSN")
	if privDSN == "" {
		t.Skip("METADATA_DSN not set; skipping live RLS tests")
	}
	appDSN := os.Getenv("METADATA_APP_DSN")
	if appDSN == "" {
		t.Skip("METADATA_APP_DSN (non-superuser role) not set; skipping RLS enforcement tests")
	}
	ctx := context.Background()

	priv := openPQ(t, privDSN)
	defer priv.Close()
	armRLSTestTable(t, priv)

	app := openPQ(t, appDSN)
	defer app.Close()

	// Refuse to run the enforcement assertions if the app DSN is secretly
	// privileged — they would pass vacuously.
	var superuser string
	if err := app.QueryRow(`SELECT current_setting('is_superuser')`).Scan(&superuser); err != nil {
		t.Fatalf("probe is_superuser: %v", err)
	}
	if superuser != "off" {
		t.Fatalf("METADATA_APP_DSN role is a superuser (is_superuser=%s); RLS would be bypassed", superuser)
	}

	appStore, err := New(Config{DB: app, Table: ciRLSTestTable})
	if err != nil {
		t.Fatalf("New(app): %v", err)
	}
	privStore, err := New(Config{DB: priv, Table: ciRLSTestTable})
	if err != nil {
		t.Fatalf("New(priv): %v", err)
	}

	// t1 registers its own entry through the non-superuser app store:
	// beginTenant binds zkof.tenant_id=t1, so WITH CHECK admits it.
	if err := appStore.Register(ctx, entryFor("t1")); err != nil {
		t.Fatalf("app Register t1: %v", err)
	}
	// t2's row is seeded via the privileged store (a real second tenant).
	if err := privStore.Register(ctx, entryFor("t2")); err != nil {
		t.Fatalf("priv Register t2: %v", err)
	}

	t.Run("non-superuser app store round-trips its own tenant under RLS", func(t *testing.T) {
		// A least-privilege role performs normal tenant-scoped work once
		// RLS is armed: read its own row and miss cleanly on an absent
		// hash. (Cross-tenant isolation is proven by the raw, predicate-
		// free checks below — the store self-binds the GUC to each call's
		// tenantID, so it cannot express a leak here.)
		got, err := appStore.Lookup(ctx, "t1", "h-t1")
		if err != nil {
			t.Fatalf("Lookup t1: %v", err)
		}
		if got.PieceID != "p-t1" {
			t.Fatalf("Lookup t1 piece_id = %q, want p-t1", got.PieceID)
		}
		if _, err := appStore.Lookup(ctx, "t1", "h-missing"); err == nil {
			t.Fatal("Lookup absent t1 hash returned nil error, want ErrNotFound")
		}
	})

	t.Run("raw query without tenant predicate is RLS-scoped", func(t *testing.T) {
		// No GUC bound at all → fail-closed: zero visible rows.
		var n int
		if err := app.QueryRowContext(ctx, `SELECT count(*) FROM `+ciRLSTestTable).Scan(&n); err != nil {
			t.Fatalf("count (no GUC): %v", err)
		}
		if n != 0 {
			t.Fatalf("no-GUC raw count = %d, want 0 (RLS must hide every row when unscoped)", n)
		}

		// Bound to t1 → sees exactly t1's row, never t2's, even though the
		// query has no WHERE tenant_id predicate.
		tx, err := appStore.beginTenant(ctx, "t1")
		if err != nil {
			t.Fatalf("beginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var tenants string
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(string_agg(DISTINCT tenant_id, ','), '') FROM `+ciRLSTestTable).Scan(&tenants); err != nil {
			t.Fatalf("scoped select: %v", err)
		}
		if tenants != "t1" {
			t.Fatalf("t1-scoped raw select saw tenants %q, want \"t1\"", tenants)
		}
	})

	t.Run("cross-tenant write is rejected by WITH CHECK", func(t *testing.T) {
		// Bind t1 but try to INSERT a t2 row directly. The app store's own
		// methods can't express this (they self-bind to the row's tenant),
		// so we issue the raw statement the policy must stop.
		tx, err := appStore.beginTenant(ctx, "t1")
		if err != nil {
			t.Fatalf("beginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+ciRLSTestTable+` (tenant_id, content_hash, piece_id, backend, ref_count)
			 VALUES ('t2', 'h-evil', 'p-evil', 'mem', 1)`)
		if err == nil {
			t.Fatal("cross-tenant INSERT under t1 scope succeeded; WITH CHECK should reject it")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("cross-tenant INSERT error = %v, want a row-level security violation", err)
		}
	})

	t.Run("scan_all bypass enumerates every tenant", func(t *testing.T) {
		// ListTenants is the orphan-GC enumerator; it binds scan_all and
		// must therefore see both tenants' rows.
		tenants, err := appStore.ListTenants(ctx)
		if err != nil {
			t.Fatalf("ListTenants: %v", err)
		}
		seen := map[string]bool{}
		for _, tn := range tenants {
			seen[tn] = true
		}
		if !seen["t1"] || !seen["t2"] {
			t.Fatalf("ListTenants saw %v, want both t1 and t2 (audited cross-tenant sweep)", seen)
		}
	})

	// Sanity: rlsdb.GUCScanAll is the GUC ListTenants binds; assert the
	// constant the policy DDL references matches what beginScanAll sets.
	if rlsdb.GUCScanAll != "zkof.scan_all" {
		t.Fatalf("unexpected GUCScanAll = %q", rlsdb.GUCScanAll)
	}
}
