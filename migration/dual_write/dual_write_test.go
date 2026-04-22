package dual_write

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// memProvider is a tiny in-memory StorageProvider used by dual-write
// tests. It records every mutation for assertions.
type memProvider struct {
	name    string
	store   map[string][]byte
	putErr  error
	getErr  error
}

func newMem(name string) *memProvider {
	return &memProvider{name: name, store: map[string][]byte{}}
}

func (m *memProvider) PutPiece(_ context.Context, id string, r io.Reader, _ providers.PutOptions) (providers.PutResult, error) {
	if m.putErr != nil {
		return providers.PutResult{}, m.putErr
	}
	data, _ := io.ReadAll(r)
	m.store[id] = data
	return providers.PutResult{PieceID: id, SizeBytes: int64(len(data)), Backend: m.name}, nil
}
func (m *memProvider) GetPiece(_ context.Context, id string, _ *providers.ByteRange) (io.ReadCloser, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
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

func TestDualWrite_Put_WritesBoth(t *testing.T) {
	ctx := context.Background()
	primary := newMem("primary")
	secondary := newMem("secondary")
	d := New("dual", primary, secondary)

	if _, err := d.PutPiece(ctx, "p1", bytes.NewReader([]byte("hello")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if string(primary.store["p1"]) != "hello" {
		t.Fatalf("primary missing payload: %q", primary.store["p1"])
	}
	if string(secondary.store["p1"]) != "hello" {
		t.Fatalf("secondary missing payload: %q", secondary.store["p1"])
	}
}

func TestDualWrite_Put_PrimaryFailureAborts(t *testing.T) {
	ctx := context.Background()
	primary := newMem("primary")
	primary.putErr = errors.New("primary down")
	secondary := newMem("secondary")
	d := New("dual", primary, secondary)

	if _, err := d.PutPiece(ctx, "p1", bytes.NewReader([]byte("hello")), providers.PutOptions{}); err == nil {
		t.Fatal("PutPiece: want error on primary failure, got nil")
	}
	if _, ok := secondary.store["p1"]; ok {
		t.Fatal("secondary should not receive the piece when primary fails")
	}
}

func TestDualWrite_Put_SecondaryFailureIsSoft(t *testing.T) {
	ctx := context.Background()
	primary := newMem("primary")
	secondary := newMem("secondary")
	secondary.putErr = errors.New("secondary down")
	d := New("dual", primary, secondary)

	if _, err := d.PutPiece(ctx, "p1", bytes.NewReader([]byte("hello")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: secondary failure should not fail request; got %v", err)
	}
	if string(primary.store["p1"]) != "hello" {
		t.Fatal("primary should still have received the piece")
	}
}

func TestDualWrite_Get_FallsBackToSecondary(t *testing.T) {
	ctx := context.Background()
	primary := newMem("primary")
	secondary := newMem("secondary")
	secondary.store["p1"] = []byte("from-secondary")
	d := New("dual", primary, secondary)

	rc, err := d.GetPiece(ctx, "p1", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "from-secondary" {
		t.Fatalf("body = %q, want from-secondary", got)
	}
}

func TestDualWrite_Delete_BestEffort(t *testing.T) {
	ctx := context.Background()
	primary := newMem("primary")
	secondary := newMem("secondary")
	primary.store["p1"] = []byte("x")
	secondary.store["p1"] = []byte("x")
	d := New("dual", primary, secondary)

	if err := d.DeletePiece(ctx, "p1"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if _, ok := primary.store["p1"]; ok {
		t.Fatal("primary entry survived delete")
	}
	if _, ok := secondary.store["p1"]; ok {
		t.Fatal("secondary entry survived delete")
	}
}
