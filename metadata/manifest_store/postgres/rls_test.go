package postgres

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/internal/rlsdb"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// RLS enforcement is a property of Postgres, not of Go, so it can only
// be verified against a live database — and only when the app connects
// as a non-superuser role (Postgres bypasses RLS for superusers and
// BYPASSRLS roles regardless of FORCE). These tests therefore use two
// DSNs:
//
//	METADATA_DSN     — a privileged role (table owner / superuser) used
//	                   to create the table, arm RLS, and seed rows.
//	METADATA_APP_DSN — the least-privilege, non-superuser role the
//	                   gateway connects as in production. The
//	                   enforcement assertions run over this connection.
//
// Both are skipped when unset so the unit suite stays hermetic.

const (
	rlsTestTable = "manifests_rls_test"
	rlsTestRole  = "zkof_rls_test"
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

// armRLSTestTable (re)creates the test table owned by the privileged
// role and arms Row-Level Security on it via the production DDL helper.
func armRLSTestTable(t *testing.T, priv *sql.DB) {
	t.Helper()
	if _, err := priv.Exec(`DROP TABLE IF EXISTS ` + rlsTestTable); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	if _, err := priv.Exec(`
		CREATE TABLE ` + rlsTestTable + ` (
			tenant_id        TEXT  NOT NULL,
			bucket           TEXT  NOT NULL,
			object_key_hash  TEXT  NOT NULL,
			version_id       TEXT  NOT NULL,
			body             JSONB NOT NULL,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, bucket, object_key_hash, version_id)
		)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	stmts, err := RLSStatements(rlsTestTable, rlsTestRole)
	if err != nil {
		t.Fatalf("RLSStatements: %v", err)
	}
	for _, s := range stmts {
		if _, err := priv.Exec(s); err != nil {
			t.Fatalf("arm RLS stmt %q: %v", s, err)
		}
	}
	t.Cleanup(func() { _, _ = priv.Exec(`DROP TABLE IF EXISTS ` + rlsTestTable) })
}

func manifestFor(tenant string) *metadata.ObjectManifest {
	return &metadata.ObjectManifest{TenantID: tenant, Bucket: "b", ObjectKey: "k"}
}

func keyFor(tenant string) manifest_store.ManifestKey {
	return manifest_store.ManifestKey{TenantID: tenant, Bucket: "b", ObjectKeyHash: "h-" + tenant, VersionID: "v1"}
}

func TestRLSStatements_Validation(t *testing.T) {
	if _, err := RLSStatements("bad-table", "zkof_app"); err == nil {
		t.Error("RLSStatements accepted an unsafe table identifier")
	}
	if _, err := RLSStatements("manifests", "zkof_app; DROP TABLE x"); err == nil {
		t.Error("RLSStatements accepted an unsafe role identifier")
	}
	stmts, err := RLSStatements("manifests", "zkof_app")
	if err != nil {
		t.Fatalf("RLSStatements(valid): %v", err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tenant_isolation",
		"current_setting('zkof.tenant_id', true)",
		"current_setting('zkof.scan_all', true)",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON manifests TO zkof_app",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("RLSStatements output missing %q\n--- got ---\n%s", want, joined)
		}
	}
	// WITH CHECK must NOT honour the scan_all bypass: even the global
	// sweep may not write a row under a foreign tenant_id. Inspect only
	// the text of the WITH CHECK clause (scan_all legitimately appears in
	// the USING clause of the same statement).
	for _, s := range stmts {
		idx := strings.Index(s, "WITH CHECK")
		if idx < 0 {
			continue
		}
		if strings.Contains(s[idx:], rlsdb.GUCScanAll) {
			t.Errorf("WITH CHECK clause must not reference %s (write bypass):\n%s", rlsdb.GUCScanAll, s[idx:])
		}
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

	// Refuse to run the enforcement assertions if the app DSN is
	// secretly privileged — they would pass vacuously.
	var superuser string
	if err := app.QueryRow(`SELECT current_setting('is_superuser')`).Scan(&superuser); err != nil {
		t.Fatalf("probe is_superuser: %v", err)
	}
	if superuser != "off" {
		t.Fatalf("METADATA_APP_DSN role is a superuser (is_superuser=%s); RLS would be bypassed", superuser)
	}

	appStore, err := New(Config{DB: app, Table: rlsTestTable})
	if err != nil {
		t.Fatalf("New(app): %v", err)
	}
	privStore, err := New(Config{DB: priv, Table: rlsTestTable})
	if err != nil {
		t.Fatalf("New(priv): %v", err)
	}

	// t1 writes its own manifest through the non-superuser app store:
	// beginTenant binds zkof.tenant_id=t1, so WITH CHECK admits it.
	if err := appStore.Put(ctx, keyFor("t1"), manifestFor("t1")); err != nil {
		t.Fatalf("app Put t1: %v", err)
	}
	// t2's row is seeded via the privileged store (a real second tenant).
	if err := privStore.Put(ctx, keyFor("t2"), manifestFor("t2")); err != nil {
		t.Fatalf("priv Put t2: %v", err)
	}

	t.Run("non-superuser app store round-trips its own tenant under RLS", func(t *testing.T) {
		// Proves a least-privilege role can perform normal tenant-scoped
		// work once RLS is armed: read its own row and miss cleanly on a
		// key it never wrote. (Cross-tenant isolation is proven by the
		// raw, predicate-free checks below — the store self-binds the GUC
		// to each call's key.TenantID, so it cannot express a leak here.)
		if _, err := appStore.Get(ctx, keyFor("t1")); err != nil {
			t.Fatalf("Get t1: %v", err)
		}
		absent := manifest_store.ManifestKey{TenantID: "t1", Bucket: "b", ObjectKeyHash: "h-missing", VersionID: "v1"}
		if _, err := appStore.Get(ctx, absent); !errors.Is(err, manifest_store.ErrNotFound) {
			t.Fatalf("Get absent t1 key: got %v, want ErrNotFound", err)
		}
	})

	t.Run("raw query without tenant predicate is RLS-scoped", func(t *testing.T) {
		// No GUC bound at all → fail-closed: zero visible rows.
		var n int
		if err := app.QueryRowContext(ctx, `SELECT count(*) FROM `+rlsTestTable).Scan(&n); err != nil {
			t.Fatalf("count (no GUC): %v", err)
		}
		if n != 0 {
			t.Fatalf("no-GUC raw count = %d, want 0 (RLS must hide every row when unscoped)", n)
		}

		// Bound to t1 → sees exactly t1's row, never t2's, even though
		// the query has no WHERE tenant_id predicate.
		tx, err := appStore.beginTenant(ctx, "t1")
		if err != nil {
			t.Fatalf("beginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var tenants string
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(string_agg(DISTINCT tenant_id, ','), '') FROM `+rlsTestTable).Scan(&tenants); err != nil {
			t.Fatalf("scoped select: %v", err)
		}
		if tenants != "t1" {
			t.Fatalf("t1-scoped raw select saw tenants %q, want \"t1\"", tenants)
		}
	})

	t.Run("cross-tenant write is rejected by WITH CHECK", func(t *testing.T) {
		// Bind t1 but try to INSERT a t2 row directly. The app store's
		// own methods can't express this (they self-bind to the row's
		// tenant), so we issue the raw statement the policy must stop.
		tx, err := appStore.beginTenant(ctx, "t1")
		if err != nil {
			t.Fatalf("beginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO `+rlsTestTable+` (tenant_id, bucket, object_key_hash, version_id, body)
			 VALUES ('t2', 'b', 'h-evil', 'v1', '{}'::jsonb)`)
		if err == nil {
			t.Fatal("cross-tenant INSERT under t1 scope succeeded; WITH CHECK should reject it")
		}
		if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
			t.Fatalf("cross-tenant INSERT error = %v, want a row-level security violation", err)
		}
	})

	t.Run("scan_all bypass sees every tenant", func(t *testing.T) {
		res, err := appStore.ScanManifests(ctx, "", 100)
		if err != nil {
			t.Fatalf("ScanManifests: %v", err)
		}
		seen := map[string]bool{}
		for _, m := range res.Manifests {
			seen[m.Key.TenantID] = true
		}
		if !seen["t1"] || !seen["t2"] {
			t.Fatalf("ScanManifests saw tenants %v, want both t1 and t2 (audited cross-tenant sweep)", seen)
		}
	})
}
