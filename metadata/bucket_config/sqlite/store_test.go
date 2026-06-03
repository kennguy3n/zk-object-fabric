package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "bc.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(Config{DB: db})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestSQLite_SetGetRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetVersioning(ctx, "t1", "b1"); err != nil || got != bucket_config.VersioningUnset {
		t.Fatalf("unconfigured get = (%q, %v), want (unset, nil)", got, err)
	}
	if err := s.SetVersioning(ctx, "t1", "b1", bucket_config.VersioningEnabled); err != nil {
		t.Fatalf("SetVersioning: %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != bucket_config.VersioningEnabled {
		t.Fatalf("state = %q, want Enabled", got)
	}
	// Upsert path.
	if err := s.SetVersioning(ctx, "t1", "b1", bucket_config.VersioningSuspended); err != nil {
		t.Fatalf("SetVersioning(upsert): %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != bucket_config.VersioningSuspended {
		t.Fatalf("state = %q, want Suspended", got)
	}
}

func TestSQLite_RejectsInvalidState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetVersioning(context.Background(), "t1", "b1", bucket_config.VersioningUnset); err == nil {
		t.Fatal("SetVersioning(unset): want error")
	}
}
