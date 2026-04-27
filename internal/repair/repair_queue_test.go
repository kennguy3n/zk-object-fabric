package repair

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

type fakeSource struct {
	signal HealthSignal
	err    error
}

func (f *fakeSource) Poll(_ context.Context) (HealthSignal, error) { return f.signal, f.err }

type fakeScanner struct {
	manifests []*metadata.ObjectManifest
}

func (f *fakeScanner) FindManifestsByPieceID(_ context.Context, _ []string) ([]*metadata.ObjectManifest, error) {
	return f.manifests, nil
}

func mkProv(t *testing.T, root string) providers.StorageProvider {
	t.Helper()
	p, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRepairQueue_HealthyPollIsNoOp(t *testing.T) {
	q := NewRepairQueue(
		&fakeSource{signal: HealthSignal{Healthy: true}},
		&fakeScanner{},
		memory.New(),
		map[string]providers.StorageProvider{},
		erasure_coding.DefaultRegistry(),
	)
	q.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = q.Run(ctx)
	if q.RepairedCount() != 0 {
		t.Errorf("RepairedCount = %d, want 0", q.RepairedCount())
	}
}

func TestRepairQueue_SkipsNonECManifests(t *testing.T) {
	m := &metadata.ObjectManifest{
		TenantID: "T", Bucket: "b", ObjectKey: "k", ObjectKeyHash: "k",
		Pieces: []metadata.Piece{{PieceID: "p1"}},
	}
	q := NewRepairQueue(
		&fakeSource{signal: HealthSignal{Healthy: false, AffectedPieceIDs: []string{"p1"}}},
		&fakeScanner{manifests: []*metadata.ObjectManifest{m}},
		memory.New(),
		map[string]providers.StorageProvider{},
		erasure_coding.DefaultRegistry(),
	)
	q.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = q.Run(ctx)
	if q.RepairedCount() != 0 {
		t.Errorf("non-EC manifest must not be repaired, got %d", q.RepairedCount())
	}
}

func TestRepairQueue_ReencodesECManifest(t *testing.T) {
	tmp := t.TempDir()
	prov := mkProv(t, filepath.Join(tmp, "src"))
	reg := erasure_coding.DefaultRegistry()
	encoderRegistry := reg
	profileName := encoderRegistry.Names()[0]
	enc, err := reg.Lookup(profileName)
	if err != nil {
		t.Fatal(err)
	}
	plaintext := bytes.Repeat([]byte("zk-object-fabric:repair-test\n"), 64)
	shards, err := enc.Encode(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	pieces := make([]metadata.Piece, 0, len(shards))
	ctx := context.Background()
	for i, sh := range shards {
		pid := pieceID(i)
		pieces = append(pieces, metadata.Piece{
			PieceID:     pid,
			Backend:     "src",
			StripeIndex: sh.StripeIndex,
			ShardIndex:  sh.ShardIndex,
			ShardKind:   sh.Kind.String(),
			SizeBytes:   int64(len(sh.Bytes)),
		})
		if _, err := prov.PutPiece(ctx, pid, bytes.NewReader(sh.Bytes), providers.PutOptions{ContentLength: int64(len(sh.Bytes))}); err != nil {
			t.Fatal(err)
		}
	}
	m := &metadata.ObjectManifest{
		TenantID: "T", Bucket: "b", ObjectKey: "k", ObjectKeyHash: "k",
		ObjectSize: int64(len(plaintext)),
		Pieces:     pieces,
		PlacementPolicy: metadata.PlacementPolicy{
			ErasureProfile: profileName,
		},
	}
	// Simulate a degraded shard by deleting it from the provider.
	degraded := pieces[0].PieceID
	if err := prov.DeletePiece(ctx, degraded); err != nil {
		t.Fatal(err)
	}
	store := memory.New()
	if err := store.Put(ctx, manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "k"}, m); err != nil {
		t.Fatal(err)
	}
	q := NewRepairQueue(
		&fakeSource{signal: HealthSignal{Healthy: false, AffectedPieceIDs: []string{degraded}}},
		&fakeScanner{manifests: []*metadata.ObjectManifest{m}},
		store,
		map[string]providers.StorageProvider{"src": prov},
		reg,
	)
	q.PollInterval = 10 * time.Millisecond
	cctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_ = q.Run(cctx)
	if q.RepairedCount() < 1 {
		t.Fatalf("RepairedCount = %d, want >= 1 (failed=%d)", q.RepairedCount(), q.FailedCount())
	}
	rc, err := prov.GetPiece(context.Background(), degraded, nil)
	if err != nil {
		t.Fatalf("repaired piece missing: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !bytes.Equal(body, shards[0].Bytes) {
		t.Errorf("repaired body differs from original shard")
	}
}

func TestRepairQueue_PollErrorIsRecorded(t *testing.T) {
	q := NewRepairQueue(
		&fakeSource{err: errors.New("ceph down")},
		&fakeScanner{},
		memory.New(),
		map[string]providers.StorageProvider{},
		erasure_coding.DefaultRegistry(),
	)
	q.PollInterval = 10 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = q.Run(ctx)
	if q.LastPollAt().IsZero() {
		t.Errorf("LastPollAt should be set even on error")
	}
}

func pieceID(i int) string {
	return "shard-" + string(rune('a'+i))
}
