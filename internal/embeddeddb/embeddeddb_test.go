package embeddeddb

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesParentDirAndPragmas(t *testing.T) {
	t.Parallel()
	// Path under a not-yet-existing subdirectory exercises the
	// MkdirAll branch (fresh-volume case).
	path := filepath.Join(t.TempDir(), "nested", "dir", "embedded.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	var journal string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if journal != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journal)
	}
	var fk int
	if err := db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("foreign_keys = %d, want 1", fk)
	}

	// Round-trip a row to prove the handle is usable.
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (v) VALUES (?)`, "x"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var got string
	if err := db.QueryRow(`SELECT v FROM t WHERE id = 1`).Scan(&got); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got != "x" {
		t.Fatalf("v = %q, want x", got)
	}
}

func TestOpenEmptyPathErrors(t *testing.T) {
	t.Parallel()
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") = nil error, want error")
	}
}

func TestOpenPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "embedded.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (42)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	db2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	var id int
	if err := db2.QueryRow(`SELECT id FROM t`).Scan(&id); err != nil {
		t.Fatalf("select after reopen: %v", err)
	}
	if id != 42 {
		t.Fatalf("id = %d, want 42", id)
	}
}

func TestOpenMemoryIsUsable(t *testing.T) {
	t.Parallel()
	db, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
		t.Fatalf("insert: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}
