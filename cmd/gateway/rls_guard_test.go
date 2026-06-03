// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	_ "github.com/lib/pq"
)

// The non-DB branches of checkProductionRLSRole are hermetic; the
// superuser/non-superuser branches require a live Postgres and are gated
// on the same DSNs as the manifest-store RLS tests:
//
//	METADATA_DSN     — a privileged (superuser/owner) connection.
//	METADATA_APP_DSN — a least-privilege, non-superuser role.

func TestCheckProductionRLSRole_NonProduction(t *testing.T) {
	// Even with a nil DB, a non-production env must never probe or fail.
	if err := checkProductionRLSRole(context.Background(), "development", nil); err != nil {
		t.Fatalf("checkProductionRLSRole(development) = %v, want nil", err)
	}
}

func TestCheckProductionRLSRole_NoMetadataDB(t *testing.T) {
	// Embedded / in-memory profile: no Postgres, so RLS does not apply
	// and production startup must not be blocked.
	if err := checkProductionRLSRole(context.Background(), "production", nil); err != nil {
		t.Fatalf("checkProductionRLSRole(production, nil DB) = %v, want nil", err)
	}
}

func TestCheckProductionRLSRole_SuperuserFails(t *testing.T) {
	dsn := os.Getenv("METADATA_DSN")
	if dsn == "" {
		t.Skip("METADATA_DSN not set; skipping live superuser RLS-guard test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	// METADATA_DSN is the privileged role; the guard must refuse it.
	if err := checkProductionRLSRole(context.Background(), "production", db); !errors.Is(err, errProductionRLSRoleInert) {
		t.Fatalf("checkProductionRLSRole(superuser) = %v, want errors.Is(_, errProductionRLSRoleInert)", err)
	}
}

func TestCheckProductionRLSRole_NonSuperuserPasses(t *testing.T) {
	dsn := os.Getenv("METADATA_APP_DSN")
	if dsn == "" {
		t.Skip("METADATA_APP_DSN (non-superuser role) not set; skipping live RLS-guard pass test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := checkProductionRLSRole(context.Background(), "production", db); err != nil {
		t.Fatalf("checkProductionRLSRole(non-superuser) = %v, want nil", err)
	}
}
