// Tests for the background AAD v1 migration worker.
//
// The worker upgrades legacy gateway-encrypted objects
// (AADVersion == "") to v1 in place. These tests drive it against the
// in-memory manifest store and the package's fakeProvider so the full
// re-encrypt → switch → reclaim cycle can be asserted end-to-end:
//
//   - an eligible legacy single-piece object is re-sealed v1, its
//     version_id is preserved, the new ciphertext round-trips under the
//     object's own identity (and fails closed under a wrong identity),
//     and the old backend piece is reclaimed;
//   - the migration is idempotent — a second sweep is a no-op;
//   - ineligible objects (client_side, already-v1, convergent/dedup,
//     multi-piece, oversize) are skipped with the right accounting and
//     never touched;
//   - the worker is a no-op when disabled.
package s3compat

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// nopLogger discards worker log lines so test output stays quiet.
type nopLogger struct{}

func (nopLogger) Printf(string, ...any) {}

// newMigratorHandler wires a Handler with gateway encryption, a fake
// backend, and an in-memory manifest store — the minimum the migration
// worker exercises end-to-end.
func newMigratorHandler(t *testing.T) (*Handler, *fakeProvider, manifest_store.ManifestStore) {
	t.Helper()
	cmkPath := filepath.Join(t.TempDir(), "cmk.key")
	cmk := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(cmk); err != nil {
		t.Fatalf("rand cmk: %v", err)
	}
	if err := os.WriteFile(cmkPath, cmk, 0o600); err != nil {
		t.Fatalf("write cmk: %v", err)
	}
	fake := newFakeProvider("test")
	store := memory.New()
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"test": fake},
		Encryption: &GatewayEncryption{
			Wrapper: client_sdk.LocalFileWrapper{Path: cmkPath},
			CMK: encryption.CustomerMasterKeyRef{
				URI:         "cmk://test/primary",
				Version:     1,
				HolderClass: "gateway_hsm",
			},
		},
	})
	return h, fake, store
}

// putLegacyObject seals plaintext with nil AAD, stores the piece on the
// fake backend, and writes a legacy (AADVersion == "") single-piece
// manifest. It returns the store key so callers can re-Get it.
func putLegacyObject(t *testing.T, h *Handler, fake *fakeProvider, store manifest_store.ManifestStore, tenant, bucket, key, version string, plaintext []byte) manifest_store.ManifestKey {
	t.Helper()
	ciphertext, legacyEnc := sealLegacy(t, h, plaintext)
	pieceID := "legacy-piece-" + key + "-" + version
	fake.setPiece(pieceID, ciphertext)

	hash := hashObjectKey(key)
	mkey := manifest_store.ManifestKey{TenantID: tenant, Bucket: bucket, ObjectKeyHash: hash, VersionID: version}
	man := &metadata.ObjectManifest{
		TenantID:      tenant,
		Bucket:        bucket,
		ObjectKey:     key,
		ObjectKeyHash: hash,
		VersionID:     version,
		ObjectSize:    int64(len(plaintext)),
		// Mirror production: the single-piece PUT path records the
		// stored chunk size from putRes.SizeBytes (ciphertext, which
		// includes AEAD overhead), not the plaintext length.
		ChunkSize:     int64(len(ciphertext)),
		Encryption:    legacyEnc,
		Pieces:        []metadata.Piece{{PieceID: pieceID, Backend: "test", State: "active", SizeBytes: int64(len(ciphertext))}},
	}
	if err := store.Put(context.Background(), mkey, man); err != nil {
		t.Fatalf("seed legacy manifest: %v", err)
	}
	return mkey
}

func TestAADMigrator_MigratesLegacySinglePiece(t *testing.T) {
	h, fake, store := newMigratorHandler(t)
	ctx := context.Background()
	plaintext := []byte("legacy managed object that must converge on AAD v1 in place")

	mkey := putLegacyObject(t, h, fake, store, "tenant-a", "bucket-a", "doc.txt", "v-0001", plaintext)
	oldPieceID := func() string {
		m, _ := store.Get(ctx, mkey)
		return m.Pieces[0].PieceID
	}()

	mig, err := NewAADMigrator(h, AADMigratorConfig{Logger: nopLogger{}})
	if err != nil {
		t.Fatalf("NewAADMigrator: %v", err)
	}
	stats, err := mig.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Scanned != 1 || stats.Migrated != 1 {
		t.Fatalf("stats = %+v, want scanned=1 migrated=1", stats)
	}

	got, err := store.Get(ctx, mkey)
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	// Identity must be preserved: same key (version_id) so the GET
	// path rebuilds the identical AAD.
	if got.VersionID != "v-0001" {
		t.Fatalf("version_id changed by migration: %q", got.VersionID)
	}
	if got.Encryption.AADVersion != AADVersionV1 {
		t.Fatalf("AADVersion = %q, want v1", got.Encryption.AADVersion)
	}
	if got.Encryption.Mode != "managed" {
		t.Fatalf("Mode not preserved: %q", got.Encryption.Mode)
	}
	if len(got.Pieces) != 1 || got.Pieces[0].PieceID == oldPieceID {
		t.Fatalf("piece not rewritten under a new id: %+v (old=%s)", got.Pieces, oldPieceID)
	}
	// ChunkSize must track the freshly stored piece, not the inherited
	// pre-migration value (mirrors the live PUT path).
	if got.ChunkSize != got.Pieces[0].SizeBytes {
		t.Fatalf("ChunkSize=%d not pinned to new piece SizeBytes=%d", got.ChunkSize, got.Pieces[0].SizeBytes)
	}

	// Old piece reclaimed.
	if _, ok := fake.pieceBytes(oldPieceID); ok {
		t.Fatalf("old piece %s was not deleted after migration", oldPieceID)
	}

	// The new ciphertext must open under the object's own identity
	// (the GET path) and fail under any other — proving the v1 bind.
	newCT, _ := fake.pieceBytes(got.Pieces[0].PieceID)
	if len(newCT) == 0 {
		t.Fatalf("new piece %s missing on backend", got.Pieces[0].PieceID)
	}
	round, derr := h.decryptFromStorage(newCT, got.Encryption, aadIdentityOf(got))
	if derr != nil {
		t.Fatalf("decrypt migrated piece under its own identity: %v", derr)
	}
	if string(round) != string(plaintext) {
		t.Fatalf("round-trip mismatch: want %q got %q", plaintext, round)
	}
	wrong := aadIdentityOf(got)
	wrong.VersionID = "v-OTHER"
	if _, derr := h.decryptFromStorage(newCT, got.Encryption, wrong); derr == nil {
		t.Fatal("migrated piece opened under a mismatched identity: want AEAD failure, got nil")
	}
}

func TestAADMigrator_Idempotent(t *testing.T) {
	h, fake, store := newMigratorHandler(t)
	ctx := context.Background()
	plaintext := []byte("second sweep must be a no-op")
	mkey := putLegacyObject(t, h, fake, store, "tenant-a", "bucket-a", "k", "v-1", plaintext)

	mig, _ := NewAADMigrator(h, AADMigratorConfig{Logger: nopLogger{}})
	if _, err := mig.Sweep(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	after, _ := store.Get(ctx, mkey)
	firstPiece := after.Pieces[0].PieceID

	stats, err := mig.Sweep(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if stats.Migrated != 0 || stats.SkippedAlready != 1 {
		t.Fatalf("second sweep stats = %+v, want migrated=0 skippedAlready=1", stats)
	}
	again, _ := store.Get(ctx, mkey)
	if again.Pieces[0].PieceID != firstPiece {
		t.Fatalf("idempotent sweep rewrote the piece: %s -> %s", firstPiece, again.Pieces[0].PieceID)
	}
}

func TestAADMigrator_Eligibility(t *testing.T) {
	cases := []struct {
		name string
		man  *metadata.ObjectManifest
		want migrateOutcome
	}{
		{
			name: "eligible legacy managed single-piece",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "managed", AADVersion: ""},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeMigrated,
		},
		{
			name: "client_side not gateway-encrypted",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "client_side", AADVersion: ""},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeNotMine,
		},
		{
			name: "legacy plaintext mode is not gateway-encrypted",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "", AADVersion: ""},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeNotMine,
		},
		{
			name: "already v1",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "managed", AADVersion: AADVersionV1},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeAlready,
		},
		{
			name: "unknown future AADVersion left alone",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "managed", AADVersion: "v2"},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeAlready,
		},
		{
			name: "convergent dedup object cannot bind AAD",
			man: &metadata.ObjectManifest{
				ContentHash: "blake3:abc",
				Encryption:  metadata.EncryptionConfig{Mode: "managed", AADVersion: ""},
				Pieces:      []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeDedup,
		},
		{
			name: "multi-piece EC/multipart deferred",
			man: &metadata.ObjectManifest{
				Encryption: metadata.EncryptionConfig{Mode: "public_distribution", AADVersion: ""},
				Pieces:     []metadata.Piece{{PieceID: "p1"}, {PieceID: "p2"}},
			},
			want: outcomeMulti,
		},
		{
			name: "oversize beyond in-memory ceiling",
			man: &metadata.ObjectManifest{
				ObjectSize: MaxInMemoryObjectBytes + 1,
				Encryption: metadata.EncryptionConfig{Mode: "managed", AADVersion: ""},
				Pieces:     []metadata.Piece{{PieceID: "p"}},
			},
			want: outcomeTooBig,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := eligibility(tc.man); got != tc.want {
				t.Fatalf("eligibility = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestAADMigrator_SkipsIneligibleEndToEnd seeds a convergent object and
// a multi-piece object alongside an eligible one and asserts the sweep
// migrates only the eligible one, leaving the others' pieces untouched.
func TestAADMigrator_SkipsIneligibleEndToEnd(t *testing.T) {
	h, fake, store := newMigratorHandler(t)
	ctx := context.Background()

	// Eligible.
	eligibleKey := putLegacyObject(t, h, fake, store, "tenant-a", "bucket-a", "eligible", "v-1", []byte("migrate me"))

	// Convergent (dedup): legacy seal but ContentHash set.
	dedupCT, dedupEnc := sealLegacy(t, h, []byte("dedup payload"))
	fake.setPiece("dedup-piece", dedupCT)
	dedupKey := manifest_store.ManifestKey{TenantID: "tenant-a", Bucket: "bucket-a", ObjectKeyHash: hashObjectKey("dedup"), VersionID: "v-1"}
	if err := store.Put(ctx, dedupKey, &metadata.ObjectManifest{
		TenantID: "tenant-a", Bucket: "bucket-a", ObjectKey: "dedup", ObjectKeyHash: dedupKey.ObjectKeyHash, VersionID: "v-1",
		ContentHash: "blake3:deadbeef",
		Encryption:  dedupEnc,
		Pieces:      []metadata.Piece{{PieceID: "dedup-piece", Backend: "test", State: "active"}},
	}); err != nil {
		t.Fatalf("seed dedup: %v", err)
	}

	// Multi-piece. Seed dummy bytes so the post-sweep presence check
	// is meaningful (the worker must never read or delete them).
	fake.setPiece("m1", []byte("shard-1"))
	fake.setPiece("m2", []byte("shard-2"))
	multiKey := manifest_store.ManifestKey{TenantID: "tenant-a", Bucket: "bucket-a", ObjectKeyHash: hashObjectKey("multi"), VersionID: "v-1"}
	if err := store.Put(ctx, multiKey, &metadata.ObjectManifest{
		TenantID: "tenant-a", Bucket: "bucket-a", ObjectKey: "multi", ObjectKeyHash: multiKey.ObjectKeyHash, VersionID: "v-1",
		Encryption: metadata.EncryptionConfig{Mode: "managed", AADVersion: ""},
		Pieces:     []metadata.Piece{{PieceID: "m1", Backend: "test", State: "active"}, {PieceID: "m2", Backend: "test", State: "active"}},
	}); err != nil {
		t.Fatalf("seed multi: %v", err)
	}

	mig, _ := NewAADMigrator(h, AADMigratorConfig{Logger: nopLogger{}})
	stats, err := mig.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Scanned != 3 || stats.Migrated != 1 || stats.SkippedDedup != 1 || stats.SkippedMulti != 1 {
		t.Fatalf("stats = %+v, want scanned=3 migrated=1 dedup=1 multi=1", stats)
	}

	// Dedup and multi pieces untouched.
	if _, ok := fake.pieceBytes("dedup-piece"); !ok {
		t.Fatal("dedup piece was deleted; convergent objects must be left alone")
	}
	if _, ok := fake.pieceBytes("m1"); !ok {
		t.Fatal("multi-piece m1 was deleted; multi-piece objects must be left alone")
	}
	dedupAfter, _ := store.Get(ctx, dedupKey)
	if dedupAfter.Encryption.AADVersion != "" {
		t.Fatal("dedup manifest was upgraded; convergent objects cannot carry v1 AAD")
	}

	// Eligible one migrated.
	eligAfter, _ := store.Get(ctx, eligibleKey)
	if eligAfter.Encryption.AADVersion != AADVersionV1 {
		t.Fatal("eligible object was not migrated")
	}
}

func TestAADMigrator_DisabledRunReturns(t *testing.T) {
	h, _, _ := newMigratorHandler(t)
	mig, _ := NewAADMigrator(h, AADMigratorConfig{Interval: 0, Logger: nopLogger{}})
	// A disabled worker must return immediately rather than block,
	// so wiring it in without an interval is safe.
	if err := mig.Run(context.Background()); err != nil {
		t.Fatalf("disabled Run returned error: %v", err)
	}
}

func TestAADMigrator_RunSweepsImmediately(t *testing.T) {
	h, fake, store := newMigratorHandler(t)
	eligibleKey := putLegacyObject(t, h, fake, store, "tenant-a", "bucket-a", "eligible", "v-1", []byte("migrate me"))

	// A long interval guarantees the ticker never fires during the
	// test: if the object is upgraded, it can only be from the
	// immediate first sweep that Run performs before entering the
	// ticker loop.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	mig, err := NewAADMigrator(h, AADMigratorConfig{Interval: time.Hour, Logger: nopLogger{}})
	if err != nil {
		t.Fatalf("NewAADMigrator: %v", err)
	}
	go func() { runErr <- mig.Run(ctx) }()

	deadline := time.After(2 * time.Second)
	for {
		man, err := store.Get(ctx, eligibleKey)
		if err == nil && man.Encryption.AADVersion == AADVersionV1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("object was not migrated by the immediate first sweep within 2s")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

func TestAADMigrator_ScanPagination(t *testing.T) {
	h, fake, store := newMigratorHandler(t)
	ctx := context.Background()
	const n = 25
	for i := 0; i < n; i++ {
		putLegacyObject(t, h, fake, store, "tenant-a", "bucket-a", "obj-"+itoaPad(i), "v-1", []byte("payload "+itoaPad(i)))
	}
	mig, _ := NewAADMigrator(h, AADMigratorConfig{PageSize: 4, Logger: nopLogger{}})
	stats, err := mig.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.Scanned != n || stats.Migrated != n {
		t.Fatalf("stats = %+v, want scanned=%d migrated=%d", stats, n, n)
	}
}

// itoaPad renders i as a zero-padded 3-digit string so the object keys
// (and thus their key hashes) sort deterministically across pages.
func itoaPad(i int) string {
	b := []byte{'0', '0', '0'}
	for p := 2; p >= 0 && i > 0; p-- {
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b)
}
