package lazy_read_repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

type memProvider struct {
	name  string
	store map[string][]byte
}

func newMem(name string) *memProvider {
	return &memProvider{name: name, store: map[string][]byte{}}
}

func (m *memProvider) PutPiece(_ context.Context, id string, r io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	data, _ := io.ReadAll(r)
	m.store[id] = data
	return providers.PutResult{PieceID: id, SizeBytes: int64(len(data)), Backend: m.name, Locator: m.name + "://" + id}, nil
}
func (m *memProvider) GetPiece(_ context.Context, id string, _ *providers.ByteRange) (io.ReadCloser, error) {
	data, ok := m.store[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
func (m *memProvider) HeadPiece(_ context.Context, id string) (providers.PieceMetadata, error) {
	data, ok := m.store[id]
	if !ok {
		return providers.PieceMetadata{}, errors.New("not found")
	}
	return providers.PieceMetadata{PieceID: id, SizeBytes: int64(len(data))}, nil
}
func (m *memProvider) DeletePiece(_ context.Context, id string) error {
	delete(m.store, id)
	return nil
}
func (m *memProvider) ListPieces(_ context.Context, _, _ string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}
func (m *memProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{SupportsRangeReads: true}
}
func (m *memProvider) CostModel() providers.ProviderCostModel { return providers.ProviderCostModel{} }
func (m *memProvider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{Provider: m.name}
}

func hashOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestRepair_CopiesFromSourceToPrimary(t *testing.T) {
	ctx := context.Background()
	old := newMem("wasabi")
	newP := newMem("ceph")
	payload := []byte("zk-piece-bytes")
	old.store["piece-1"] = payload
	registry := map[string]providers.StorageProvider{"wasabi": old, "ceph": newP}

	store := memory.New()
	key := manifest_store.ManifestKey{TenantID: "t", Bucket: "b", ObjectKeyHash: "h", VersionID: "v"}
	manifest := &metadata.ObjectManifest{
		TenantID:      "t",
		Bucket:        "b",
		ObjectKeyHash: "h",
		VersionID:     "v",
		ObjectSize:    int64(len(payload)),
		ChunkSize:     int64(len(payload)),
		Pieces: []metadata.Piece{{
			PieceID: "piece-1",
			Hash:    hashOf(payload),
			Backend: "wasabi",
			Locator: "wasabi://piece-1",
			State:   "active",
		}},
		MigrationState: metadata.MigrationState{Generation: 2, PrimaryBackend: "ceph"},
	}
	if err := store.Put(ctx, key, manifest); err != nil {
		t.Fatalf("seed manifest: %v", err)
	}

	rr := New(registry, store)
	res, err := rr.Repair(ctx, key, manifest, 0)
	if err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if string(res.Body) != string(payload) {
		t.Fatalf("Body = %q, want %q", res.Body, payload)
	}
	if string(newP.store["piece-1"]) != string(payload) {
		t.Fatalf("new primary missing piece: %q", newP.store["piece-1"])
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("reload manifest: %v", err)
	}
	if got.Pieces[0].Backend != "ceph" {
		t.Fatalf("manifest backend = %q, want ceph", got.Pieces[0].Backend)
	}
}

func TestRepair_RejectsHashMismatch(t *testing.T) {
	ctx := context.Background()
	old := newMem("wasabi")
	newP := newMem("ceph")
	old.store["piece-1"] = []byte("tampered")
	registry := map[string]providers.StorageProvider{"wasabi": old, "ceph": newP}
	store := memory.New()
	key := manifest_store.ManifestKey{TenantID: "t", Bucket: "b", ObjectKeyHash: "h", VersionID: "v"}
	manifest := &metadata.ObjectManifest{
		TenantID:      "t",
		Bucket:        "b",
		ObjectKeyHash: "h",
		VersionID:     "v",
		ObjectSize:    5,
		ChunkSize:     5,
		Pieces: []metadata.Piece{{
			PieceID: "piece-1",
			Hash:    hashOf([]byte("hello")),
			Backend: "wasabi",
		}},
		MigrationState: metadata.MigrationState{Generation: 2, PrimaryBackend: "ceph"},
	}
	_ = store.Put(ctx, key, manifest)

	rr := New(registry, store)
	if _, err := rr.Repair(ctx, key, manifest, 0); err == nil {
		t.Fatal("Repair: want hash mismatch error, got nil")
	}
	if _, ok := newP.store["piece-1"]; ok {
		t.Fatal("new primary should not have received tampered piece")
	}
}

func TestRepair_NoopWhenAlreadyOnPrimary(t *testing.T) {
	ctx := context.Background()
	registry := map[string]providers.StorageProvider{"ceph": newMem("ceph")}
	store := memory.New()
	key := manifest_store.ManifestKey{TenantID: "t", Bucket: "b", ObjectKeyHash: "h", VersionID: "v"}
	manifest := &metadata.ObjectManifest{
		TenantID:      "t",
		Bucket:        "b",
		ObjectKeyHash: "h",
		VersionID:     "v",
		Pieces: []metadata.Piece{{
			PieceID: "piece-1",
			Backend: "ceph",
		}},
		MigrationState: metadata.MigrationState{Generation: 1, PrimaryBackend: "ceph"},
	}
	rr := New(registry, store)
	if _, err := rr.Repair(ctx, key, manifest, 0); !errors.Is(err, ErrRepairUnavailable) {
		t.Fatalf("Repair: want ErrRepairUnavailable, got %v", err)
	}
}
