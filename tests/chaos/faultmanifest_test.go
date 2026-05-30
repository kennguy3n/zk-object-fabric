package chaos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
)

func mkKey(suffix string) manifest_store.ManifestKey {
	return manifest_store.ManifestKey{
		TenantID:      "T",
		Bucket:        "b",
		ObjectKeyHash: "obj-" + suffix,
		VersionID:     "v1",
	}
}

func mkManifest(suffix string) *metadata.ObjectManifest {
	return &metadata.ObjectManifest{
		TenantID:      "T",
		Bucket:        "b",
		ObjectKey:     "obj-" + suffix,
		ObjectKeyHash: "obj-" + suffix,
		ObjectSize:    1024,
		Pieces: []metadata.Piece{
			{PieceID: "piece-" + suffix, Backend: "local", SizeBytes: 1024},
		},
	}
}

func TestFaultManifestStore_NoneIsPassThrough(t *testing.T) {
	t.Parallel()
	inner := memory.New()
	fms := NewFaultManifestStore(inner)
	ctx := context.Background()
	if err := fms.Put(ctx, mkKey("1"), mkManifest("1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := fms.Get(ctx, mkKey("1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ObjectKey != "obj-1" {
		t.Errorf("ObjectKey = %q, want obj-1", got.ObjectKey)
	}
	if fms.Failures.Load() != 0 {
		t.Errorf("Failures = %d, want 0", fms.Failures.Load())
	}
}

func TestFaultManifestStore_PutFailsButGetStillReads(t *testing.T) {
	t.Parallel()
	inner := memory.New()
	ctx := context.Background()
	// Seed a manifest via the unwrapped store so we can prove Get
	// passes through even when the wrapped Put is hard-faulted —
	// this is the "Postgres primary down, replicas serving reads"
	// shape.
	if err := inner.Put(ctx, mkKey("1"), mkManifest("1")); err != nil {
		t.Fatalf("seed Put: %v", err)
	}

	fms := NewFaultManifestStore(inner)
	fms.PutFault = FaultConfig{Mode: ModeAlwaysFail, Err: errors.New("primary down")}

	if err := fms.Put(ctx, mkKey("2"), mkManifest("2")); err == nil {
		t.Error("Put under fault: want error, got nil")
	}
	got, err := fms.Get(ctx, mkKey("1"))
	if err != nil {
		t.Fatalf("Get under read-only mode: %v", err)
	}
	if got.ObjectKey != "obj-1" {
		t.Errorf("Get returned wrong manifest: %q", got.ObjectKey)
	}
	if fms.Failures.Load() != 1 {
		t.Errorf("Failures = %d, want 1 (the faulted Put)", fms.Failures.Load())
	}
}

func TestFaultManifestStore_FailEveryNthIndependentCountersPerOp(t *testing.T) {
	t.Parallel()
	inner := memory.New()
	ctx := context.Background()
	if err := inner.Put(ctx, mkKey("1"), mkManifest("1")); err != nil {
		t.Fatalf("seed: %v", err)
	}

	fms := NewFaultManifestStore(inner)
	fms.GetFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 2}
	fms.PutFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 2}

	// Interleaving Get and Put should not let either operation's
	// counter advance the other one's cadence — the counters are
	// per-op.
	for i := 0; i < 4; i++ {
		_, _ = fms.Get(ctx, mkKey("1"))
		_ = fms.Put(ctx, mkKey("k"), mkManifest("k"))
	}
	// 4 Gets => 2 fail (calls 2, 4). 4 Puts => 2 fail (calls 2, 4).
	if got, want := fms.Failures.Load(), int64(4); got != want {
		t.Errorf("Failures = %d, want %d", got, want)
	}
}

func TestFaultManifestStore_FailUntilTimeHealsAtDeadline(t *testing.T) {
	t.Parallel()
	inner := memory.New()
	ctx := context.Background()

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fms := NewFaultManifestStore(inner)
	fms.Now = func() time.Time { return now }
	fms.PutFault = FaultConfig{Mode: ModeFailUntilTime, FailUntil: now.Add(time.Minute)}

	if err := fms.Put(ctx, mkKey("1"), mkManifest("1")); err == nil {
		t.Error("Put pre-heal: want error")
	}
	// Advance now past the heal time.
	now = now.Add(2 * time.Minute)
	if err := fms.Put(ctx, mkKey("1"), mkManifest("1")); err != nil {
		t.Errorf("Put post-heal: %v", err)
	}
}

func TestFaultManifestStore_NilInnerYieldsClearError(t *testing.T) {
	t.Parallel()
	fms := &FaultManifestStore{}
	fms.PutFault = FaultConfig{Mode: ModeAlwaysFail}
	if err := fms.Put(context.Background(), mkKey("1"), mkManifest("1")); err == nil {
		t.Fatal("Put on nil Inner: want error, got nil")
	}
}
