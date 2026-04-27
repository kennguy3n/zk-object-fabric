package content_index_test

import (
	"context"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
)

// fakeProvider is a PieceDeleter that records every DeletePiece call.
type fakeProvider struct {
	deleted []string
	err     error
}

func (p *fakeProvider) DeletePiece(_ context.Context, pieceID string) error {
	p.deleted = append(p.deleted, pieceID)
	return p.err
}

type silentLogger struct{}

func (silentLogger) Printf(string, ...any) {}

func makeManifest(tenantID, bucket, key, pieceID string) *metadata.ObjectManifest {
	return &metadata.ObjectManifest{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKey:     key,
		ObjectKeyHash: key,
		VersionID:     pieceID,
		Pieces: []metadata.Piece{
			{PieceID: pieceID, Backend: "test"},
		},
	}
}

func TestOrphanGC_DeletesOrphanRow(t *testing.T) {
	idx := content_index.NewMemoryStore()
	ms := memory.New()
	prov := &fakeProvider{}

	ctx := context.Background()
	// Live entry: referenced by a manifest.
	if err := idx.Register(ctx, content_index.ContentIndexEntry{
		TenantID:    "t1",
		ContentHash: "live",
		PieceID:     "piece-live",
		Backend:     "test",
	}); err != nil {
		t.Fatalf("register live: %v", err)
	}
	if err := ms.Put(ctx, manifest_store.ManifestKey{
		TenantID:      "t1",
		Bucket:        "b",
		ObjectKeyHash: "live-key",
		VersionID:     "v1",
	}, makeManifest("t1", "b", "live-key", "piece-live")); err != nil {
		t.Fatalf("put manifest: %v", err)
	}
	// Orphan entry: no manifest references piece-orphan.
	if err := idx.Register(ctx, content_index.ContentIndexEntry{
		TenantID:    "t1",
		ContentHash: "orphan",
		PieceID:     "piece-orphan",
		Backend:     "test",
	}); err != nil {
		t.Fatalf("register orphan: %v", err)
	}

	gc, err := content_index.NewOrphanGC(content_index.OrphanGCConfig{
		Index:     idx,
		Manifests: ms,
		Resolver: func(backend string) (content_index.PieceDeleter, bool) {
			if backend == "test" {
				return prov, true
			}
			return nil, false
		},
		Logger: silentLogger{},
	})
	if err != nil {
		t.Fatalf("NewOrphanGC: %v", err)
	}
	stats, err := gc.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if stats.RowsScanned != 2 {
		t.Fatalf("RowsScanned = %d, want 2", stats.RowsScanned)
	}
	if stats.RowsOrphaned != 1 {
		t.Fatalf("RowsOrphaned = %d, want 1", stats.RowsOrphaned)
	}
	if stats.PiecesDeleted != 1 {
		t.Fatalf("PiecesDeleted = %d, want 1", stats.PiecesDeleted)
	}
	if stats.IndexRowsDeleted != 1 {
		t.Fatalf("IndexRowsDeleted = %d, want 1", stats.IndexRowsDeleted)
	}
	if len(prov.deleted) != 1 || prov.deleted[0] != "piece-orphan" {
		t.Fatalf("deleted = %v, want [piece-orphan]", prov.deleted)
	}

	// Orphan row gone, live row still present.
	if _, err := idx.Lookup(ctx, "t1", "orphan"); err == nil {
		t.Fatal("orphan row still present after sweep")
	}
	if _, err := idx.Lookup(ctx, "t1", "live"); err != nil {
		t.Fatalf("live row vanished: %v", err)
	}
}

func TestOrphanGC_PerTenantIsolation(t *testing.T) {
	idx := content_index.NewMemoryStore()
	ms := memory.New()
	prov := &fakeProvider{}
	ctx := context.Background()

	// Tenant A has an orphan, tenant B has a live entry pointing
	// at the same content_hash but a different piece.
	if err := idx.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "a", ContentHash: "h", PieceID: "piece-a", Backend: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "b", ContentHash: "h", PieceID: "piece-b", Backend: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ms.Put(ctx, manifest_store.ManifestKey{
		TenantID: "b", Bucket: "bk", ObjectKeyHash: "k", VersionID: "v",
	}, makeManifest("b", "bk", "k", "piece-b")); err != nil {
		t.Fatal(err)
	}

	gc, _ := content_index.NewOrphanGC(content_index.OrphanGCConfig{
		Index:     idx,
		Manifests: ms,
		Resolver: func(string) (content_index.PieceDeleter, bool) { return prov, true },
		Logger:   silentLogger{},
	})
	stats, err := gc.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.PiecesDeleted != 1 || stats.IndexRowsDeleted != 1 {
		t.Fatalf("stats = %+v, want one orphan removed", stats)
	}
	if _, err := idx.Lookup(ctx, "a", "h"); err == nil {
		t.Fatal("tenant a orphan still present")
	}
	if _, err := idx.Lookup(ctx, "b", "h"); err != nil {
		t.Fatalf("tenant b live entry vanished: %v", err)
	}
}

func TestOrphanGC_ContextCancellation(t *testing.T) {
	idx := content_index.NewMemoryStore()
	ms := memory.New()
	prov := &fakeProvider{}
	for i := 0; i < 5; i++ {
		_ = idx.Register(context.Background(), content_index.ContentIndexEntry{
			TenantID: "t", ContentHash: string(rune('a' + i)), PieceID: "p", Backend: "test",
		})
	}
	gc, _ := content_index.NewOrphanGC(content_index.OrphanGCConfig{
		Index:     idx,
		Manifests: ms,
		Resolver: func(string) (content_index.PieceDeleter, bool) { return prov, true },
		Logger:   silentLogger{},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := gc.Sweep(ctx); err == nil {
		t.Fatal("expected context error from cancelled sweep")
	}
}

func TestOrphanGC_MissingProviderStillDeletesRow(t *testing.T) {
	idx := content_index.NewMemoryStore()
	ms := memory.New()
	ctx := context.Background()
	_ = idx.Register(ctx, content_index.ContentIndexEntry{
		TenantID: "t", ContentHash: "h", PieceID: "p", Backend: "missing",
	})
	gc, _ := content_index.NewOrphanGC(content_index.OrphanGCConfig{
		Index:     idx,
		Manifests: ms,
		Resolver:  func(string) (content_index.PieceDeleter, bool) { return nil, false },
		Logger:    silentLogger{},
	})
	stats, err := gc.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.IndexRowsDeleted != 1 {
		t.Fatalf("IndexRowsDeleted = %d, want 1", stats.IndexRowsDeleted)
	}
	if stats.PiecesDeleted != 0 {
		t.Fatalf("PiecesDeleted = %d, want 0 (provider missing)", stats.PiecesDeleted)
	}
}
