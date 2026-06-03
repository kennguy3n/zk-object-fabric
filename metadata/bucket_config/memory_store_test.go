package bucket_config

import (
	"context"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

func TestMemoryStore_GetUnsetIsDefault(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	got, err := s.GetVersioning(context.Background(), "t1", "b1")
	if err != nil {
		t.Fatalf("GetVersioning: %v", err)
	}
	if got != VersioningUnset {
		t.Fatalf("unconfigured bucket = %q, want VersioningUnset", got)
	}
}

func TestMemoryStore_SetGetRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()

	if err := s.SetVersioning(ctx, "t1", "b1", VersioningEnabled); err != nil {
		t.Fatalf("SetVersioning(Enabled): %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != VersioningEnabled {
		t.Fatalf("state = %q, want Enabled", got)
	}

	// Overwrite with Suspended.
	if err := s.SetVersioning(ctx, "t1", "b1", VersioningSuspended); err != nil {
		t.Fatalf("SetVersioning(Suspended): %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != VersioningSuspended {
		t.Fatalf("state = %q, want Suspended", got)
	}
}

func TestMemoryStore_TenantAndBucketIsolation(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetVersioning(ctx, "t1", "b1", VersioningEnabled); err != nil {
		t.Fatalf("SetVersioning: %v", err)
	}
	// Different bucket, same tenant: unset.
	if got, _ := s.GetVersioning(ctx, "t1", "b2"); got != VersioningUnset {
		t.Fatalf("t1/b2 = %q, want VersioningUnset", got)
	}
	// Same bucket name, different tenant: unset.
	if got, _ := s.GetVersioning(ctx, "t2", "b1"); got != VersioningUnset {
		t.Fatalf("t2/b1 = %q, want VersioningUnset", got)
	}
}

func TestMemoryStore_RejectsInvalidState(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	if err := s.SetVersioning(context.Background(), "t1", "b1", VersioningUnset); err == nil {
		t.Fatal("SetVersioning(VersioningUnset): want error, got nil")
	}
}

func TestMemoryStore_RequiresTenantAndBucket(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.GetVersioning(ctx, "", "b1"); err == nil {
		t.Fatal("GetVersioning(empty tenant): want error")
	}
	if err := s.SetVersioning(ctx, "t1", "", VersioningEnabled); err == nil {
		t.Fatal("SetVersioning(empty bucket): want error")
	}
}

func TestMemoryStore_ObjectLockRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()

	// Unconfigured bucket → zero Config (disabled), nil error.
	got, err := s.GetObjectLock(ctx, "t1", "b1")
	if err != nil {
		t.Fatalf("GetObjectLock: %v", err)
	}
	if got.Enabled {
		t.Fatalf("unconfigured bucket = %+v, want disabled", got)
	}

	cfg := object_lock.Config{Enabled: true, DefaultMode: object_lock.ModeGovernance, DefaultDays: 30}
	if err := s.SetObjectLock(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetObjectLock: %v", err)
	}
	got, _ = s.GetObjectLock(ctx, "t1", "b1")
	if got != cfg {
		t.Fatalf("round-trip = %+v, want %+v", got, cfg)
	}

	// Tenant/bucket isolation.
	if other, _ := s.GetObjectLock(ctx, "t1", "b2"); other.Enabled {
		t.Fatalf("t1/b2 leaked config: %+v", other)
	}
	if other, _ := s.GetObjectLock(ctx, "t2", "b1"); other.Enabled {
		t.Fatalf("t2/b1 leaked config: %+v", other)
	}
}

func TestMemoryStore_ObjectLockValidatesAndRequiresKeys(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	// Invalid config is rejected.
	bad := object_lock.Config{Enabled: true, DefaultMode: object_lock.ModeGovernance, DefaultDays: 1, DefaultYears: 1}
	if err := s.SetObjectLock(ctx, "t1", "b1", bad); err == nil {
		t.Fatal("SetObjectLock(invalid): want error")
	}
	if _, err := s.GetObjectLock(ctx, "", "b1"); err == nil {
		t.Fatal("GetObjectLock(empty tenant): want error")
	}
	if err := s.SetObjectLock(ctx, "t1", "", object_lock.Config{Enabled: true}); err == nil {
		t.Fatal("SetObjectLock(empty bucket): want error")
	}
}

func TestMemoryStore_CORSRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()

	// Unconfigured bucket → empty Config, nil error.
	got, err := s.GetCORS(ctx, "t1", "b1")
	if err != nil {
		t.Fatalf("GetCORS: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("unconfigured bucket = %+v, want empty", got)
	}

	cfg := cors.Config{Rules: []cors.Rule{{
		AllowedOrigins: []string{"https://app.example.com"},
		AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"*"},
		ExposeHeaders:  []string{"ETag"},
		MaxAgeSeconds:  3000,
	}}}
	if err := s.SetCORS(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetCORS: %v", err)
	}
	got, _ = s.GetCORS(ctx, "t1", "b1")
	if len(got.Rules) != 1 || got.Rules[0].MaxAgeSeconds != 3000 {
		t.Fatalf("round-trip = %+v", got)
	}

	// Isolation.
	if other, _ := s.GetCORS(ctx, "t1", "b2"); !other.Empty() {
		t.Fatalf("t1/b2 leaked config: %+v", other)
	}
	if other, _ := s.GetCORS(ctx, "t2", "b1"); !other.Empty() {
		t.Fatalf("t2/b1 leaked config: %+v", other)
	}

	// Delete is idempotent.
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

func TestMemoryStore_CORSDeepCopy(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	origins := []string{"https://app.example.com"}
	cfg := cors.Config{Rules: []cors.Rule{{
		AllowedOrigins: origins,
		AllowedMethods: []string{"GET"},
	}}}
	if err := s.SetCORS(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetCORS: %v", err)
	}
	// Mutating the caller's slice after Set must not corrupt the store.
	origins[0] = "https://evil.example.com"
	cfg.Rules[0].AllowedMethods[0] = "DELETE"

	got, _ := s.GetCORS(ctx, "t1", "b1")
	if got.Rules[0].AllowedOrigins[0] != "https://app.example.com" {
		t.Fatalf("stored origin was mutated by caller: %q", got.Rules[0].AllowedOrigins[0])
	}
	if got.Rules[0].AllowedMethods[0] != "GET" {
		t.Fatalf("stored method was mutated by caller: %q", got.Rules[0].AllowedMethods[0])
	}
	// Mutating the returned copy must not corrupt the store either.
	got.Rules[0].AllowedOrigins[0] = "https://other.example.com"
	again, _ := s.GetCORS(ctx, "t1", "b1")
	if again.Rules[0].AllowedOrigins[0] != "https://app.example.com" {
		t.Fatalf("stored origin was mutated via returned copy: %q", again.Rules[0].AllowedOrigins[0])
	}
}

func TestMemoryStore_CORSValidatesAndRequiresKeys(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	// Invalid config (no rules) is rejected.
	if err := s.SetCORS(ctx, "t1", "b1", cors.Config{}); err == nil {
		t.Fatal("SetCORS(empty config): want error")
	}
	if _, err := s.GetCORS(ctx, "", "b1"); err == nil {
		t.Fatal("GetCORS(empty tenant): want error")
	}
	if err := s.DeleteCORS(ctx, "t1", ""); err == nil {
		t.Fatal("DeleteCORS(empty bucket): want error")
	}
}

func TestMemoryStore_LifecycleRoundTrip(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()

	// Unconfigured bucket → empty Config, nil error.
	got, err := s.GetLifecycle(ctx, "t1", "b1")
	if err != nil {
		t.Fatalf("GetLifecycle: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("unconfigured bucket = %+v, want empty", got)
	}

	cfg := lifecycle.Config{Rules: []lifecycle.Rule{{
		ID:         "expire-logs",
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Prefix: "logs/"},
		Expiration: &lifecycle.Expiration{Days: 30},
	}}}
	if err := s.SetLifecycle(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	got, _ = s.GetLifecycle(ctx, "t1", "b1")
	if len(got.Rules) != 1 || got.Rules[0].Expiration.Days != 30 {
		t.Fatalf("round-trip = %+v", got)
	}

	// Isolation.
	if other, _ := s.GetLifecycle(ctx, "t1", "b2"); !other.Empty() {
		t.Fatalf("t1/b2 leaked config: %+v", other)
	}
	if other, _ := s.GetLifecycle(ctx, "t2", "b1"); !other.Empty() {
		t.Fatalf("t2/b1 leaked config: %+v", other)
	}

	// ListLifecycle enumerates across tenants.
	if err := s.SetLifecycle(ctx, "t2", "bx", cfg); err != nil {
		t.Fatalf("SetLifecycle t2: %v", err)
	}
	entries, err := s.ListLifecycle(ctx)
	if err != nil {
		t.Fatalf("ListLifecycle: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListLifecycle len = %d, want 2", len(entries))
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.TenantID+"/"+e.Bucket] = true
		if e.Config.Empty() {
			t.Fatalf("entry %s/%s has empty config", e.TenantID, e.Bucket)
		}
	}
	if !seen["t1/b1"] || !seen["t2/bx"] {
		t.Fatalf("ListLifecycle missing entries: %v", seen)
	}

	// Delete is idempotent.
	if err := s.DeleteLifecycle(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteLifecycle: %v", err)
	}
	if got, _ := s.GetLifecycle(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after delete = %+v, want empty", got)
	}
	if err := s.DeleteLifecycle(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteLifecycle (no-op): %v", err)
	}
}

func TestMemoryStore_LifecycleDeepCopy(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	tags := map[string]string{"team": "infra"}
	cfg := lifecycle.Config{Rules: []lifecycle.Rule{{
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Tags: tags},
		Expiration: &lifecycle.Expiration{Days: 1},
	}}}
	if err := s.SetLifecycle(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	// Mutating the caller's map after Set must not corrupt the store.
	tags["team"] = "evil"
	cfg.Rules[0].Expiration.Days = 999

	got, _ := s.GetLifecycle(ctx, "t1", "b1")
	if got.Rules[0].Filter.Tags["team"] != "infra" {
		t.Fatalf("stored tag mutated by caller: %q", got.Rules[0].Filter.Tags["team"])
	}
	if got.Rules[0].Expiration.Days != 1 {
		t.Fatalf("stored expiration mutated by caller: %d", got.Rules[0].Expiration.Days)
	}
	// Mutating the returned copy must not corrupt the store either.
	got.Rules[0].Filter.Tags["team"] = "other"
	again, _ := s.GetLifecycle(ctx, "t1", "b1")
	if again.Rules[0].Filter.Tags["team"] != "infra" {
		t.Fatalf("stored tag mutated via returned copy: %q", again.Rules[0].Filter.Tags["team"])
	}
}

func TestMemoryStore_LifecycleValidatesAndRequiresKeys(t *testing.T) {
	t.Parallel()
	s := NewMemoryStore()
	ctx := context.Background()
	// Invalid config (no rules) is rejected.
	if err := s.SetLifecycle(ctx, "t1", "b1", lifecycle.Config{}); err == nil {
		t.Fatal("SetLifecycle(empty config): want error")
	}
	if _, err := s.GetLifecycle(ctx, "", "b1"); err == nil {
		t.Fatal("GetLifecycle(empty tenant): want error")
	}
	if err := s.DeleteLifecycle(ctx, "t1", ""); err == nil {
		t.Fatal("DeleteLifecycle(empty bucket): want error")
	}
}

func TestVersioningState_Valid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state VersioningState
		want  bool
	}{
		{VersioningEnabled, true},
		{VersioningSuspended, true},
		{VersioningUnset, false},
		{VersioningState("bogus"), false},
	} {
		if got := tc.state.Valid(); got != tc.want {
			t.Errorf("%q.Valid() = %v, want %v", tc.state, got, tc.want)
		}
	}
}
