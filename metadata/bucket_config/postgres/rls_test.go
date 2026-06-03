package postgres

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
)

// RLS enforcement is a property of Postgres, not of Go, so it can only be
// verified against a live database — and only when the app connects as a
// non-superuser role (Postgres bypasses RLS for superusers and BYPASSRLS
// roles regardless of FORCE). These tests therefore use two DSNs:
//
//	METADATA_DSN     — a privileged role used to create the tables, arm
//	                   RLS, and seed a second tenant's rows.
//	METADATA_APP_DSN — the least-privilege, non-superuser role the
//	                   gateway connects as in production. The enforcement
//	                   assertions run over this connection.
//
// Both are skipped when unset so the unit suite stays hermetic.

const ciRLSTestRole = "zkof_rls_test"

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

// armRLSTestTables (re)creates the four bucket_config tables owned by the
// privileged role under unique test names and arms Row-Level Security on
// each via the production DDL helper. It returns a Config wired to those
// table names.
func armRLSTestTables(t *testing.T, priv *sql.DB) Config {
	t.Helper()
	cfg := Config{
		DB:             priv,
		Table:          "bucket_versioning_rls_test",
		LockTable:      "bucket_object_lock_rls_test",
		CorsTable:      "bucket_cors_rls_test",
		LifecycleTable: "bucket_lifecycle_rls_test",
	}
	ddl := map[string]string{
		cfg.Table: `(
			tenant_id  TEXT NOT NULL,
			bucket     TEXT NOT NULL,
			state      TEXT NOT NULL CHECK (state IN ('Enabled', 'Suspended')),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, bucket))`,
		cfg.LockTable: `(
			tenant_id     TEXT NOT NULL,
			bucket        TEXT NOT NULL,
			enabled       BOOLEAN NOT NULL,
			default_mode  TEXT NOT NULL DEFAULT '',
			default_days  INTEGER NOT NULL DEFAULT 0,
			default_years INTEGER NOT NULL DEFAULT 0,
			updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, bucket))`,
		cfg.CorsTable: `(
			tenant_id  TEXT NOT NULL,
			bucket     TEXT NOT NULL,
			rules      TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, bucket))`,
		cfg.LifecycleTable: `(
			tenant_id  TEXT NOT NULL,
			bucket     TEXT NOT NULL,
			rules      TEXT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (tenant_id, bucket))`,
	}
	for _, table := range []string{cfg.Table, cfg.LockTable, cfg.CorsTable, cfg.LifecycleTable} {
		if _, err := priv.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
		if _, err := priv.Exec(`CREATE TABLE ` + table + ` ` + ddl[table]); err != nil {
			t.Fatalf("create %s: %v", table, err)
		}
		stmts, err := RLSStatements(table, ciRLSTestRole)
		if err != nil {
			t.Fatalf("RLSStatements(%s): %v", table, err)
		}
		for _, s := range stmts {
			if _, err := priv.Exec(s); err != nil {
				t.Fatalf("arm RLS on %s stmt %q: %v", table, s, err)
			}
		}
		table := table
		t.Cleanup(func() { _, _ = priv.Exec(`DROP TABLE IF EXISTS ` + table) })
	}
	return cfg
}

func TestRLSStatements_Alias(t *testing.T) {
	// The package-local alias must delegate to rlsdb.Statements and carry
	// the bucket_config table name through.
	stmts, err := RLSStatements("bucket_versioning", "zkof_app")
	if err != nil {
		t.Fatalf("RLSStatements: %v", err)
	}
	joined := strings.Join(stmts, "\n")
	if !strings.Contains(joined, "CREATE POLICY tenant_isolation ON bucket_versioning") {
		t.Errorf("alias output missing bucket_versioning policy:\n%s", joined)
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
	cfg := armRLSTestTables(t, priv)

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

	appCfg := cfg
	appCfg.DB = app
	appStore, err := New(appCfg)
	if err != nil {
		t.Fatalf("New(app): %v", err)
	}
	privStore, err := New(cfg)
	if err != nil {
		t.Fatalf("New(priv): %v", err)
	}

	// t1 writes its own versioning row through the non-superuser app store
	// (beginTenant binds zkof.tenant_id=t1, so WITH CHECK admits it); t2's
	// row is seeded via the privileged store as a real second tenant.
	if err := appStore.SetVersioning(ctx, "t1", "b1", bucket_config.VersioningEnabled); err != nil {
		t.Fatalf("app SetVersioning t1: %v", err)
	}
	if err := privStore.SetVersioning(ctx, "t2", "b2", bucket_config.VersioningEnabled); err != nil {
		t.Fatalf("priv SetVersioning t2: %v", err)
	}

	t.Run("non-superuser app store round-trips its own tenant under RLS", func(t *testing.T) {
		got, err := appStore.GetVersioning(ctx, "t1", "b1")
		if err != nil {
			t.Fatalf("GetVersioning t1: %v", err)
		}
		if got != bucket_config.VersioningEnabled {
			t.Fatalf("GetVersioning t1 = %q, want Enabled", got)
		}
		// A bucket the tenant never configured reads as Unset, not an error.
		if got, err := appStore.GetVersioning(ctx, "t1", "absent"); err != nil || got != bucket_config.VersioningUnset {
			t.Fatalf("GetVersioning absent = (%q, %v), want (Unset, nil)", got, err)
		}
	})

	t.Run("raw query without tenant predicate is RLS-scoped", func(t *testing.T) {
		// No GUC bound at all → fail-closed: zero visible rows.
		var n int
		if err := app.QueryRowContext(ctx, `SELECT count(*) FROM `+cfg.Table).Scan(&n); err != nil {
			t.Fatalf("count (no GUC): %v", err)
		}
		if n != 0 {
			t.Fatalf("no-GUC raw count = %d, want 0 (RLS must hide every row when unscoped)", n)
		}

		// Bound to t1 → sees exactly t1's row, never t2's.
		tx, err := appStore.beginTenant(ctx, "t1")
		if err != nil {
			t.Fatalf("beginTenant t1: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		var tenants string
		if err := tx.QueryRowContext(ctx,
			`SELECT coalesce(string_agg(DISTINCT tenant_id, ','), '') FROM `+cfg.Table).Scan(&tenants); err != nil {
			t.Fatalf("scoped select: %v", err)
		}
		if tenants != "t1" {
			t.Fatalf("t1-scoped raw select saw tenants %q, want \"t1\"", tenants)
		}
	})

	t.Run("cross-tenant write is rejected by WITH CHECK on every table", func(t *testing.T) {
		// Bind t1 but try to INSERT a t2 row directly into each of the four
		// tables. The store's own methods can't express this (they
		// self-bind to the call's tenant), so we issue the raw statements
		// the policy must stop. Exercising all four guards against future
		// per-table schema drift where one table's policy is accidentally
		// weakened. Each INSERT lists exactly the NOT NULL columns that
		// have no default, so the row is constructible and the failure is
		// unambiguously the RLS WITH CHECK (not a constraint violation).
		foreignInserts := []struct {
			table string
			cols  string
			vals  string
		}{
			{cfg.Table, "tenant_id, bucket, state", "'t2', 'evil', 'Enabled'"},
			{cfg.LockTable, "tenant_id, bucket, enabled", "'t2', 'evil', false"},
			{cfg.CorsTable, "tenant_id, bucket, rules", "'t2', 'evil', '{}'"},
			{cfg.LifecycleTable, "tenant_id, bucket, rules", "'t2', 'evil', '{}'"},
		}
		for _, fi := range foreignInserts {
			fi := fi
			t.Run(fi.table, func(t *testing.T) {
				tx, err := appStore.beginTenant(ctx, "t1")
				if err != nil {
					t.Fatalf("beginTenant t1: %v", err)
				}
				defer func() { _ = tx.Rollback() }()
				_, err = tx.ExecContext(ctx,
					`INSERT INTO `+fi.table+` (`+fi.cols+`) VALUES (`+fi.vals+`)`)
				if err == nil {
					t.Fatalf("cross-tenant INSERT into %s under t1 scope succeeded; WITH CHECK should reject it", fi.table)
				}
				if !strings.Contains(strings.ToLower(err.Error()), "row-level security") {
					t.Fatalf("cross-tenant INSERT into %s error = %v, want a row-level security violation", fi.table, err)
				}
			})
		}
	})

	t.Run("scan_all bypass enumerates lifecycle across every tenant", func(t *testing.T) {
		// Seed lifecycle rows for two tenants via the privileged role
		// (an empty rules document unmarshals to a zero Config).
		for _, tn := range []string{"t1", "t2"} {
			if _, err := priv.ExecContext(ctx,
				`INSERT INTO `+cfg.LifecycleTable+` (tenant_id, bucket, rules) VALUES ($1, 'b', '{}')`, tn); err != nil {
				t.Fatalf("seed lifecycle %s: %v", tn, err)
			}
		}
		// ListLifecycle binds scan_all, so the non-superuser app store must
		// see both tenants' rows even though a normal tenant-scoped read
		// would hide t2 from t1.
		entries, err := appStore.ListLifecycle(ctx)
		if err != nil {
			t.Fatalf("ListLifecycle: %v", err)
		}
		seen := map[string]bool{}
		for _, e := range entries {
			seen[e.TenantID] = true
		}
		if !seen["t1"] || !seen["t2"] {
			t.Fatalf("ListLifecycle saw %v, want both t1 and t2 (audited cross-tenant sweep)", seen)
		}
	})
}
