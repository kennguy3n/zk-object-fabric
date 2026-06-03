package bucket_config

import (
	"context"
	"testing"
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
