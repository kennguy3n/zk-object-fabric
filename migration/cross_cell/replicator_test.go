package cross_cell

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

func mkProvider(t *testing.T, root string) providers.StorageProvider {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func putPiece(t *testing.T, p providers.StorageProvider, id string, body []byte) {
	t.Helper()
	if _, err := p.PutPiece(context.Background(), id, bytes.NewReader(body), providers.PutOptions{ContentLength: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
}

func mkCells(t *testing.T) (Cell, Cell) {
	t.Helper()
	tmp := t.TempDir()
	srcMS := memory.New()
	dstMS := memory.New()
	src := Cell{ID: "src", Manifests: srcMS, Provider: mkProvider(t, filepath.Join(tmp, "src"))}
	dst := Cell{ID: "dst", Manifests: dstMS, Provider: mkProvider(t, filepath.Join(tmp, "dst"))}
	return src, dst
}

func putManifest(t *testing.T, store manifest_store.ManifestStore, m *metadata.ObjectManifest) {
	t.Helper()
	key := manifest_store.ManifestKey{
		TenantID:      m.TenantID,
		Bucket:        m.Bucket,
		ObjectKeyHash: m.ObjectKeyHash,
		VersionID:     m.VersionID,
	}
	if err := store.Put(context.Background(), key, m); err != nil {
		t.Fatal(err)
	}
}

func newAsyncManifest(tid, bucket, keyHash, pieceID string) *metadata.ObjectManifest {
	return &metadata.ObjectManifest{
		TenantID:      tid,
		Bucket:        bucket,
		ObjectKey:     keyHash,
		ObjectKeyHash: keyHash,
		Pieces:        []metadata.Piece{{PieceID: pieceID, Backend: "src"}},
		PlacementPolicy: metadata.PlacementPolicy{
			ReplicationPolicy: &metadata.ReplicationPolicy{
				SourceCell: "src",
				DestCell:   "dst",
				Mode:       "async",
			},
		},
	}
}

func TestReplicator_CopiesAsyncManifestPiecesAndManifest(t *testing.T) {
	src, dst := mkCells(t)
	body := []byte("hello-cross-cell")
	putPiece(t, src.Provider, "p1", body)
	putManifest(t, src.Manifests, newAsyncManifest("T", "b", "k1", "p1"))

	r := NewReplicator(src, dst, []ScopeKey{{TenantID: "T", Bucket: "b"}})
	r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	got, err := dst.Provider.GetPiece(context.Background(), "p1", nil)
	if err != nil {
		t.Fatalf("dst missing piece: %v", err)
	}
	defer got.Close()
	out, _ := io.ReadAll(got)
	if !bytes.Equal(out, body) {
		t.Errorf("dst body = %q, want %q", out, body)
	}
	if r.CopiedPieces() < 1 {
		t.Errorf("CopiedPieces = %d, want >= 1", r.CopiedPieces())
	}
	if _, err := dst.Manifests.Get(context.Background(), manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "k1"}); err != nil {
		t.Errorf("dst manifest missing: %v", err)
	}
}

func TestReplicator_SkipsManifestsWithoutReplicationPolicy(t *testing.T) {
	src, dst := mkCells(t)
	putPiece(t, src.Provider, "p1", []byte("x"))
	m := newAsyncManifest("T", "b", "k1", "p1")
	m.PlacementPolicy.ReplicationPolicy = nil
	putManifest(t, src.Manifests, m)

	r := NewReplicator(src, dst, []ScopeKey{{TenantID: "T", Bucket: "b"}})
	r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if _, err := dst.Provider.GetPiece(context.Background(), "p1", nil); err == nil {
		t.Errorf("dst should not have piece for non-replicated manifest")
	}
	if r.CopiedPieces() != 0 {
		t.Errorf("CopiedPieces = %d, want 0", r.CopiedPieces())
	}
}

func TestReplicator_SkipsSyncMode(t *testing.T) {
	src, dst := mkCells(t)
	putPiece(t, src.Provider, "p1", []byte("x"))
	m := newAsyncManifest("T", "b", "k1", "p1")
	m.PlacementPolicy.ReplicationPolicy.Mode = "sync"
	putManifest(t, src.Manifests, m)

	r := NewReplicator(src, dst, []ScopeKey{{TenantID: "T", Bucket: "b"}})
	r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if r.CopiedPieces() != 0 {
		t.Errorf("sync mode must be ignored, got %d copies", r.CopiedPieces())
	}
}

func TestReplicator_FiltersByCellID(t *testing.T) {
	src, dst := mkCells(t)
	putPiece(t, src.Provider, "p1", []byte("x"))
	m := newAsyncManifest("T", "b", "k1", "p1")
	m.PlacementPolicy.ReplicationPolicy.SourceCell = "other"
	putManifest(t, src.Manifests, m)

	r := NewReplicator(src, dst, []ScopeKey{{TenantID: "T", Bucket: "b"}})
	r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)

	if r.CopiedPieces() != 0 {
		t.Errorf("source-cell mismatch must be ignored, got %d copies", r.CopiedPieces())
	}
}

func TestReplicator_LagIsRecorded(t *testing.T) {
	src, dst := mkCells(t)
	r := NewReplicator(src, dst, []ScopeKey{{TenantID: "T", Bucket: "b"}})
	r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = r.Run(ctx)
	if r.LagNanos() <= 0 {
		t.Errorf("LagNanos should record tick duration, got %d", r.LagNanos())
	}
}
