package background_rebalancer

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zeebo/blake3"

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

// TestThrottledReader_CancellationUnblocksWaitN asserts that
// cancelling ctx while throttledReader.Read is waiting on the
// rate limiter unblocks the read promptly. Before the streaming
// rebalancer landed the rebalancer slept once per piece in a
// post-copy throttle(); now the throttle is inline per-Read on
// the streaming pipe, so the cancellation contract has moved
// onto the Reader. A 10 KiB/s limiter draining a 256 KiB payload
// would need ~6.4 s of WaitN sleeps in the worst case — the test
// fails closed if the read does not return within 1 s of cancel.
func TestThrottledReader_CancellationUnblocksWaitN(t *testing.T) {
	payload := make([]byte, 256*1024)
	src := bytes.NewReader(payload)
	ctx, cancel := context.WithCancel(context.Background())
	tr := newThrottledReader(ctx, src, 10*1024)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	buf := make([]byte, 256*1024)
	start := time.Now()
	_, err := io.ReadFull(tr, buf)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("throttledReader err = %v, want context.Canceled", err)
	}
	if elapsed > time.Second {
		t.Fatalf("throttledReader blocked for %v, want <1s (cancellation must unblock WaitN)", elapsed)
	}
}

// TestThrottledReader_NoLimitPassthrough asserts that the
// passthrough branch (BytesPerSecond <= 0) makes Read identical
// to the underlying reader — same byte counts, no allocations,
// no spurious WaitN waits. Without this the streaming rebalance
// path would incur token-bucket overhead on every Read for
// deployments that do not opt into a cap.
func TestThrottledReader_NoLimitPassthrough(t *testing.T) {
	payload := make([]byte, 128*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	tr := newThrottledReader(context.Background(), bytes.NewReader(payload), 0)
	got, err := io.ReadAll(tr)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("passthrough read = %d bytes, want %d (and equal)", len(got), len(payload))
	}
}

// recordingIntegritySink captures Inc / IncUnrecognized calls so
// the streaming-rebalance tests can assert exactly which metric
// fired (and for which backend) without wiring the full
// internal/metrics registry into a unit test.
type recordingIntegritySink struct {
	mu             sync.Mutex
	failures       []string
	unrecognized   []string
	failureCount   int
	unrecognizedCt int
}

func (s *recordingIntegritySink) Inc(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failures = append(s.failures, backend)
	s.failureCount++
}

func (s *recordingIntegritySink) IncUnrecognized(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unrecognized = append(s.unrecognized, backend)
	s.unrecognizedCt++
}

// blakeHash returns the BLAKE3 hash of data in the on-wire
// "blake3:<hex>" form that pieceintegrity.Verify expects on the
// Phase 4+ path. Tests use it to compose a piece whose recorded
// Hash matches the correct content, then optionally overwrite
// the piece on the source provider to simulate bit-rot.
func blakeHash(data []byte) string {
	h := blake3.New()
	_, _ = h.Write(data)
	return "blake3:" + hex.EncodeToString(h.Sum(nil))
}

// TestRebalancer_StreamingCopyVerifiesIntegrity asserts the
// streaming pipeline catches a source-side content mismatch:
// the manifest records a BLAKE3 hash of the original payload
// but the source provider has been overwritten with different
// bytes, simulating bit-rot or a tampered backend. The
// rebalancer must (a) fail the manifest copy, (b) delete the
// destination piece that received the bad bytes, and (c) emit
// the integrity-failure metric labelled with the source backend.
func TestRebalancer_StreamingCopyVerifiesIntegrity(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	source := makeFSProvider(t, "wasabi")
	primary := makeFSProvider(t, "ceph")

	good := []byte("canonical bytes the manifest hash binds to            xxx")
	bad := []byte("BIT-ROTTED bytes the source returns to the rebalancer now")
	if len(good) != len(bad) {
		t.Fatalf("good/bad must be the same length to isolate the hash mismatch")
	}
	recordedHash := blakeHash(good)

	// Seed the source with the corrupted bytes so the stream
	// returns them to the rebalancer.
	if _, err := source.PutPiece(ctx, "p-tamper", bytes.NewReader(bad), providers.PutOptions{ContentLength: int64(len(bad))}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	m := &metadata.ObjectManifest{
		TenantID:      "tenantA",
		Bucket:        "bucket1",
		ObjectKey:     "obj-tamper",
		ObjectKeyHash: "obj-tamper-hash",
		VersionID:     "obj-tamper-v1",
		ObjectSize:    int64(len(good)),
		ChunkSize:     int64(len(good)),
		MigrationState: metadata.MigrationState{
			Generation:     2,
			CloudCopy:      "wasabi",
			PrimaryBackend: "ceph",
		},
		Pieces: []metadata.Piece{{
			PieceID:   "p-tamper",
			Hash:      recordedHash,
			Backend:   "wasabi",
			State:     "active",
			SizeBytes: int64(len(good)),
		}},
	}
	if err := store.Put(ctx, manifest_store.ManifestKey{
		TenantID: m.TenantID, Bucket: m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID,
	}, m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	sink := &recordingIntegritySink{}
	reb := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"wasabi": source, "ceph": primary},
		Targets: []TenantTarget{{
			TenantID: "tenantA", Bucket: "bucket1",
			SourceBackend: "wasabi", PrimaryBackend: "ceph",
		}},
		IntegrityFailures: sink,
	})

	stats, err := reb.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Errors != 1 {
		t.Fatalf("stats.Errors = %d, want 1 (integrity failure must surface as an error)", stats.Errors)
	}
	if stats.PiecesCopied != 0 {
		t.Fatalf("stats.PiecesCopied = %d, want 0 (a hash-mismatch is not a successful copy)", stats.PiecesCopied)
	}
	if sink.failureCount != 1 {
		t.Fatalf("sink.failureCount = %d, want 1", sink.failureCount)
	}
	if sink.failures[0] != "wasabi" {
		t.Fatalf("sink.failures[0] = %q, want %q (label must be the source backend)", sink.failures[0], "wasabi")
	}
	if sink.unrecognizedCt != 0 {
		t.Fatalf("sink.unrecognizedCt = %d, want 0 (a verified mismatch is not the unrecognised-format path)", sink.unrecognizedCt)
	}

	// The destination must not retain the corrupted bytes.
	if _, err := primary.HeadPiece(ctx, "p-tamper"); err == nil {
		t.Fatalf("destination still holds tampered piece after integrity failure; rebalancer must delete")
	}
}

// TestRebalancer_StreamingCopyClean asserts the happy path of
// the streaming rebalancer: a piece whose recorded BLAKE3 hash
// matches its source bytes copies across cleanly, the manifest
// is rewritten to point at the destination backend, and no
// integrity counter fires. This test exercises an io.Copy path
// that materially exceeds the limiter's burst floor so the
// throttledReader's per-Read loop runs more than once.
func TestRebalancer_StreamingCopyClean(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	source := makeFSProvider(t, "wasabi")
	primary := makeFSProvider(t, "ceph")

	payload := make([]byte, 200*1024)
	if _, err := rand.Read(payload); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := source.PutPiece(ctx, "p-stream", bytes.NewReader(payload), providers.PutOptions{ContentLength: int64(len(payload))}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	m := &metadata.ObjectManifest{
		TenantID:      "tenantA",
		Bucket:        "bucket1",
		ObjectKey:     "obj-stream",
		ObjectKeyHash: "obj-stream-hash",
		VersionID:     "obj-stream-v1",
		ObjectSize:    int64(len(payload)),
		ChunkSize:     int64(len(payload)),
		MigrationState: metadata.MigrationState{
			Generation: 2, CloudCopy: "wasabi", PrimaryBackend: "ceph",
		},
		Pieces: []metadata.Piece{{
			PieceID:   "p-stream",
			Hash:      blakeHash(payload),
			Backend:   "wasabi",
			State:     "active",
			SizeBytes: int64(len(payload)),
		}},
	}
	if err := store.Put(ctx, manifest_store.ManifestKey{
		TenantID: m.TenantID, Bucket: m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID,
	}, m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	sink := &recordingIntegritySink{}
	reb := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"wasabi": source, "ceph": primary},
		Targets: []TenantTarget{{
			TenantID: "tenantA", Bucket: "bucket1",
			SourceBackend: "wasabi", PrimaryBackend: "ceph",
		}},
		BytesPerSecond:    1024 * 1024, // 1 MiB/s; well over the payload so the throttle is effectively free
		IntegrityFailures: sink,
	})

	stats, err := reb.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("stats.Errors = %d, want 0", stats.Errors)
	}
	if stats.PiecesCopied != 1 {
		t.Fatalf("stats.PiecesCopied = %d, want 1", stats.PiecesCopied)
	}
	if stats.BytesCopied != int64(len(payload)) {
		t.Fatalf("stats.BytesCopied = %d, want %d", stats.BytesCopied, len(payload))
	}
	if sink.failureCount != 0 || sink.unrecognizedCt != 0 {
		t.Fatalf("unexpected integrity counters: failure=%d unrecognized=%d", sink.failureCount, sink.unrecognizedCt)
	}

	// Manifest must now reference the destination backend.
	got, err := store.Get(ctx, manifest_store.ManifestKey{
		TenantID: m.TenantID, Bucket: m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID,
	})
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.Pieces[0].Backend != "ceph" {
		t.Fatalf("piece backend = %q, want %q (manifest must record the destination after copy)", got.Pieces[0].Backend, "ceph")
	}

	// Destination bytes must round-trip.
	rc, err := primary.GetPiece(ctx, "p-stream", nil)
	if err != nil {
		t.Fatalf("GetPiece on primary: %v", err)
	}
	defer rc.Close()
	dest, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read primary: %v", err)
	}
	if !bytes.Equal(dest, payload) {
		t.Fatalf("destination bytes diverge from source (streaming copy must be byte-exact)")
	}
}

// TestRebalancer_StreamingCopyUnrecognizedHash asserts the legacy
// path: a manifest whose Piece.Hash holds a non-empty opaque ETag
// (the form copy/dedup/multipart manifests stamped before the
// BLAKE3 cut-over) does NOT block the rebalance. The piece flows
// across, the destination keeps the bytes, and the
// unrecognised-claim counter fires so operators can see how many
// manifests still need a one-shot rewrite. Treating this as a
// hard error would break every legacy migration on upgrade.
func TestRebalancer_StreamingCopyUnrecognizedHash(t *testing.T) {
	ctx := context.Background()
	store := memory.New()
	source := makeFSProvider(t, "wasabi")
	primary := makeFSProvider(t, "ceph")

	payload := []byte("legacy manifest payload \u2014 hash is an opaque provider ETag")
	if _, err := source.PutPiece(ctx, "p-legacy", bytes.NewReader(payload), providers.PutOptions{ContentLength: int64(len(payload))}); err != nil {
		t.Fatalf("seed source: %v", err)
	}

	m := &metadata.ObjectManifest{
		TenantID: "tenantA", Bucket: "bucket1",
		ObjectKey: "obj-legacy", ObjectKeyHash: "obj-legacy-hash",
		VersionID: "obj-legacy-v1",
		ObjectSize: int64(len(payload)), ChunkSize: int64(len(payload)),
		MigrationState: metadata.MigrationState{
			Generation: 2, CloudCopy: "wasabi", PrimaryBackend: "ceph",
		},
		Pieces: []metadata.Piece{{
			PieceID:   "p-legacy",
			Hash:      "\"abcdef-12-multipart-etag\"", // legacy provider ETag form
			Backend:   "wasabi",
			State:     "active",
			SizeBytes: int64(len(payload)),
		}},
	}
	if err := store.Put(ctx, manifest_store.ManifestKey{
		TenantID: m.TenantID, Bucket: m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID,
	}, m); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	sink := &recordingIntegritySink{}
	reb := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"wasabi": source, "ceph": primary},
		Targets: []TenantTarget{{
			TenantID: "tenantA", Bucket: "bucket1",
			SourceBackend: "wasabi", PrimaryBackend: "ceph",
		}},
		IntegrityFailures: sink,
	})

	stats, err := reb.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Errors != 0 {
		t.Fatalf("stats.Errors = %d, want 0 (legacy hash form must not fail the copy)", stats.Errors)
	}
	if stats.PiecesCopied != 1 {
		t.Fatalf("stats.PiecesCopied = %d, want 1", stats.PiecesCopied)
	}
	if sink.failureCount != 0 {
		t.Fatalf("sink.failureCount = %d, want 0 (legacy form is not a verified mismatch)", sink.failureCount)
	}
	if sink.unrecognizedCt != 1 {
		t.Fatalf("sink.unrecognizedCt = %d, want 1", sink.unrecognizedCt)
	}
	if sink.unrecognized[0] != "wasabi" {
		t.Fatalf("sink.unrecognized[0] = %q, want %q", sink.unrecognized[0], "wasabi")
	}

	// Destination bytes still land — we cannot prove they're wrong.
	rc, err := primary.GetPiece(ctx, "p-legacy", nil)
	if err != nil {
		t.Fatalf("GetPiece on primary: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, payload) {
		t.Fatalf("legacy-hash copy lost bytes")
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
