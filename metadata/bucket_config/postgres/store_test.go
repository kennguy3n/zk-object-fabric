package postgres

import (
	"database/sql"
	"testing"
)

func TestNew_RejectsInvalidTable(t *testing.T) {
	if _, err := New(Config{DB: nil}); err == nil {
		t.Fatal("New(nil DB) = nil error, want error")
	}
}

func TestNew_RejectsInvalidLockTable(t *testing.T) {
	// A non-nil DB handle (never used) lets New get past the DB check
	// so we exercise the lockTable ident validation.
	db := &sql.DB{}
	if _, err := New(Config{DB: db, LockTable: "bad;name"}); err == nil {
		t.Fatal("New(invalid LockTable) = nil error, want error")
	}
	if _, err := New(Config{DB: db, Table: "1bad"}); err == nil {
		t.Fatal("New(invalid Table) = nil error, want error")
	}
	if _, err := New(Config{DB: db, CorsTable: "bad;name"}); err == nil {
		t.Fatal("New(invalid CorsTable) = nil error, want error")
	}
}

func TestIsSafeIdent(t *testing.T) {
	cases := map[string]bool{
		"bucket_versioning": true,
		"BucketVersioning":  true,
		"t1":                true,
		"":                  false,
		"1table":            false,
		"bad name":          false,
		"drop;table":        false,
		"a-b":               false,
	}
	for in, want := range cases {
		if got := isSafeIdent(in); got != want {
			t.Errorf("isSafeIdent(%q) = %v, want %v", in, got, want)
		}
	}
}
