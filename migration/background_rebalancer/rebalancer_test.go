package background_rebalancer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

func seedManifest(t *testing.T, store manifest_store.ManifestStore, tenantID, bucket, objectKey string, generation int, cloudCopy, backend string, pieceIDs []string) *metadata.ObjectManifest {
	t.Helper()
	m := &metadata.ObjectManifest{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKey:     objectKey,
		ObjectKeyHash: objectKey + "-hash",
		VersionID:     objectKey + "-v1",
		ObjectSize:    int64(len(pieceIDs) * 8),
		ChunkSize:     8,
		MigrationState: metadata.MigrationState{
			Generation:     generation,
			CloudCopy:      cloudCopy,
			PrimaryBackend: "ceph",
		},
	}
	for _, id := range pieceIDs {
		m.Pieces = append(m.Pieces, metadata.Piece{
			PieceID: id,
			Backend: backend,
			State:   "active",
		})
	}
	if err := store.Put(context.Background(), manifest_store.ManifestKey{
		TenantID:      m.TenantID,
		Bucket:        m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}, m); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return m
}

func makeFSProvider(t *testing.T, name string) providers.StorageProvider {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", root, err)
	}
	p, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	return p
}

func seedPiece(t *testing.T, p providers.StorageProvider, id string, data []byte) {
	t.Helper()
	if _, err := p.PutPiece(context.Background(), id, bytes.NewReader(data), providers.PutOptions{ContentLength: int64(len(data))}); err != nil {
		t.Fatalf("seed piece %s: %v", id, err)
	}
}

func TestRebalancer_StateMachineFullMigration(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	source := makeFSProvider(t, "wasabi")
	primary := makeFSProvider(t, "ceph")

	pieces := []string{"p1", "p2", "p3"}
	for _, id := range pieces {
		seedPiece(t, source, id, []byte("payload-"+id))
	}
	// Start in DualWrite (generation=2) with pieces on wasabi.
	m := seedManifest(t, store, "tenantA", "bucket1", "obj1", 2, "wasabi", "wasabi", pieces)

	reb := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"wasabi": source, "ceph": primary},
		Targets: []TenantTarget{{
			TenantID:       "tenantA",
			Bucket:         "bucket1",
			SourceBackend:  "wasabi",
			PrimaryBackend: "ceph",
		}},
	})

	// Pass 1: copies pieces from wasabi→ceph, advances DualWrite→LocalPrimaryWasabiBackup.
	stats, err := reb.Run(ctx)
	if err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if stats.PiecesCopied != len(pieces) {
		t.Fatalf("pass 1: copied %d, want %d", stats.PiecesCopied, len(pieces))
	}
	if stats.PhasesAdvanced != 1 {
		t.Fatalf("pass 1: phases advanced %d, want 1", stats.PhasesAdvanced)
	}
	got, err := store.Get(ctx, manifest_store.ManifestKey{TenantID: "tenantA", Bucket: "bucket1", ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if phase := migration.MigrationPhase(phaseOf(got)); phase != migration.LocalPrimaryWasabiBackup {
		t.Fatalf("pass 1 phase = %q, want %q", phase, migration.LocalPrimaryWasabiBackup)
	}

	// Pass 2: no pieces to copy; pieces already on primary → advance to Drain.
	stats, err = reb.Run(ctx)
	if err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if stats.PiecesCopied != 0 {
		t.Fatalf("pass 2: copied %d, want 0", stats.PiecesCopied)
	}
	if stats.PhasesAdvanced != 1 {
		t.Fatalf("pass 2: phases advanced %d, want 1", stats.PhasesAdvanced)
	}
	got, _ = store.Get(ctx, manifest_store.ManifestKey{TenantID: "tenantA", Bucket: "bucket1", ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID})
	if phase := migration.MigrationPhase(phaseOf(got)); phase != migration.LocalPrimaryWasabiDrain {
		t.Fatalf("pass 2 phase = %q, want %q", phase, migration.LocalPrimaryWasabiDrain)
	}

	// Pass 3: advance Drain → LocalOnly (CloudCopy cleared).
	stats, err = reb.Run(ctx)
	if err != nil {
		t.Fatalf("pass 3: %v", err)
	}
	if stats.PhasesAdvanced != 1 {
		t.Fatalf("pass 3: phases advanced %d, want 1", stats.PhasesAdvanced)
	}
	got, _ = store.Get(ctx, manifest_store.ManifestKey{TenantID: "tenantA", Bucket: "bucket1", ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID})
	if phase := migration.MigrationPhase(phaseOf(got)); phase != migration.LocalOnly {
		t.Fatalf("pass 3 phase = %q, want %q", phase, migration.LocalOnly)
	}

	// Pass 4: terminal; no further transitions.
	stats, err = reb.Run(ctx)
	if err != nil {
		t.Fatalf("pass 4: %v", err)
	}
	if stats.PhasesAdvanced != 0 {
		t.Fatalf("pass 4: phases advanced %d, want 0 (terminal)", stats.PhasesAdvanced)
	}
}

// TestRebalancer_ThrottleHonoursContextCancellation asserts that
// cancelling ctx while throttle is sleeping unblocks the call
// promptly instead of waiting the full bytes / BytesPerSecond
// duration. Prior to the Phase 2 hardening pass the throttle used
// time.Sleep, so a SIGTERM-driven shutdown would block for up to
// piece_size / BytesPerSecond seconds.
func TestRebalancer_ThrottleHonoursContextCancellation(t *testing.T) {
	// 1 B/s against 100 bytes would block for 100 s under
	// time.Sleep; the ctx-aware timer+select must unblock within
	// the cancellation grace window instead. The payload is kept
	// well below the ~9.2e18 ns int64 cap of time.Duration so the
	// computed sleep duration does not wrap to a non-positive
	// value (which would bypass the sleep entirely).
	reb := New(Config{
		Manifests:      memory.New(),
		Providers:      map[string]providers.StorageProvider{},
		BytesPerSecond: 1,
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := reb.throttle(ctx, 100)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("throttle err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("throttle blocked for %v, want < 1s (cancellation must unblock promptly)", elapsed)
	}
}

func TestRebalancer_NoTargetsPassThrough(t *testing.T) {
	reb := New(Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{},
	})
	stats, err := reb.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ManifestsScanned != 0 || stats.PiecesCopied != 0 {
		t.Fatalf("unexpected work: %+v", stats)
	}
}
