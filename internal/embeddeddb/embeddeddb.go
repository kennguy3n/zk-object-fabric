// Package embeddeddb opens the SQLite database that backs the
// gateway's embedded / single-node deployment profile.
//
// The embedded profile lets `docker compose up` (and local dev)
// run with durable control-plane state but without standing up a
// Postgres. It is selected when control_plane.metadata_dsn is empty
// and control_plane.embedded_db_path is set (see internal/config). The
// SQLite-backed stores in metadata/manifest_store/sqlite,
// metadata/content_index/sqlite, api/console (SQLiteAuthStore), and
// billing (SQLiteSink) all share the single *sql.DB this package
// returns.
//
// Concurrency model: the pool is capped at a single open connection
// (SetMaxOpenConns(1)). SQLite serialises writers anyway, and a
// single connection lets the stores run read-modify-write sequences
// (e.g. content_index ref-count updates, manifest write_seq bumps)
// without explicit transactions while remaining race-free. The
// embedded profile targets a single gateway process; horizontally
// scaled deployments use Postgres.
package embeddeddb

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	// modernc.org/sqlite is a pure-Go (CGO-free) SQLite driver. It
	// registers itself under the name "sqlite".
	_ "modernc.org/sqlite"
)

// Open opens (creating if necessary) the SQLite database at path and
// returns a ready-to-use *sql.DB. The parent directory is created
// when missing so a configured path like
// /var/lib/zk-object-fabric/embedded.db works on a fresh volume.
//
// Connection pragmas:
//   - journal_mode=WAL    durable, allows concurrent readers
//   - busy_timeout=5000   wait up to 5s on a locked DB instead of
//     failing immediately with SQLITE_BUSY
//   - foreign_keys=ON     enforce referential integrity
//   - synchronous=NORMAL  safe with WAL, far faster than FULL
func Open(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("embeddeddb: path is required")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("embeddeddb: create dir %q: %w", dir, err)
		}
	}
	// _pragma query parameters are applied by modernc.org/sqlite on
	// every new connection, so they hold for the lifetime of the
	// pool rather than just the first connection.
	dsn := fmt.Sprintf(
		"file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("embeddeddb: open %q: %w", path, err)
	}
	// Single connection: see the package comment for the rationale.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embeddeddb: ping %q: %w", path, err)
	}
	return db, nil
}

// OpenMemory opens a private in-memory SQLite database for tests. It
// uses the same single-connection model as Open so test behaviour
// matches the on-disk embedded profile.
func OpenMemory() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file::memory:?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, fmt.Errorf("embeddeddb: open memory: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("embeddeddb: ping memory: %w", err)
	}
	return db, nil
}
