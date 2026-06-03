package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
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

func TestSQLite_ObjectLockRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetObjectLock(ctx, "t1", "b1"); err != nil || got.Enabled {
		t.Fatalf("unconfigured get = (%+v, %v), want (disabled, nil)", got, err)
	}

	// Enabled with a years default rule.
	cfg := object_lock.Config{Enabled: true, DefaultMode: object_lock.ModeCompliance, DefaultYears: 1}
	if err := s.SetObjectLock(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetObjectLock: %v", err)
	}
	if got, _ := s.GetObjectLock(ctx, "t1", "b1"); got != cfg {
		t.Fatalf("round-trip = %+v, want %+v", got, cfg)
	}

	// Upsert to enabled-no-rule clears the default fields.
	if err := s.SetObjectLock(ctx, "t1", "b1", object_lock.Config{Enabled: true}); err != nil {
		t.Fatalf("SetObjectLock(upsert): %v", err)
	}
	got, _ := s.GetObjectLock(ctx, "t1", "b1")
	if !got.Enabled || got.DefaultMode != "" || got.DefaultDays != 0 || got.DefaultYears != 0 {
		t.Fatalf("upsert result = %+v, want enabled-no-rule", got)
	}
}

func TestSQLite_ObjectLockRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	bad := object_lock.Config{Enabled: false, DefaultDays: 5} // stray rule on disabled
	if err := s.SetObjectLock(context.Background(), "t1", "b1", bad); err == nil {
		t.Fatal("SetObjectLock(invalid): want error")
	}
}

func TestSQLite_CORSRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetCORS(ctx, "t1", "b1"); err != nil || !got.Empty() {
		t.Fatalf("unconfigured get = (%+v, %v), want (empty, nil)", got, err)
	}

	cfg := cors.Config{Rules: []cors.Rule{
		{
			ID:             "rule-1",
			AllowedOrigins: []string{"https://app.example.com", "https://*.cdn.example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  3000,
		},
		{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"HEAD"},
		},
	}}
	if err := s.SetCORS(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetCORS: %v", err)
	}
	got, _ := s.GetCORS(ctx, "t1", "b1")
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	r0 := got.Rules[0]
	if r0.ID != "rule-1" || r0.MaxAgeSeconds != 3000 ||
		len(r0.AllowedOrigins) != 2 || r0.AllowedOrigins[1] != "https://*.cdn.example.com" ||
		r0.AllowedMethodsCSV() != "GET, PUT" || r0.ExposeHeadersCSV() != "ETag" {
		t.Fatalf("rule 0 round-trip mismatch: %+v", r0)
	}

	// Upsert replaces the rule set.
	if err := s.SetCORS(ctx, "t1", "b1", cors.Config{Rules: []cors.Rule{{
		AllowedOrigins: []string{"https://only.example.com"},
		AllowedMethods: []string{"GET"},
	}}}); err != nil {
		t.Fatalf("SetCORS(upsert): %v", err)
	}
	if got, _ := s.GetCORS(ctx, "t1", "b1"); len(got.Rules) != 1 || got.Rules[0].AllowedOrigins[0] != "https://only.example.com" {
		t.Fatalf("upsert result = %+v", got)
	}

	// Delete then idempotent re-delete.
	if err := s.DeleteCORS(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteCORS: %v", err)
	}
	if got, _ := s.GetCORS(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after delete = %+v, want empty", got)
	}
	if err := s.DeleteCORS(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteCORS (no-op): %v", err)
	}
}

func TestSQLite_CORSRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetCORS(context.Background(), "t1", "b1", cors.Config{}); err == nil {
		t.Fatal("SetCORS(empty config): want error")
	}
}
