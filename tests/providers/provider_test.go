// Package providers_test is the StorageProvider conformance suite.
//
// Any provider that implements providers.StorageProvider must pass
// this suite. It is run against local_fs_dev here so it can execute
// without cloud credentials; the same suite can be pointed at wasabi
// (and future adapters) in Phase 2.
package providers_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// newProvider returns a StorageProvider rooted in a fresh temp dir.
// Phase 2 will take a provider factory from the test harness so the
// same suite can be pointed at Wasabi or any other backend.
func newProvider(t *testing.T) providers.StorageProvider {
	t.Helper()
	root := filepath.Join(t.TempDir(), "pieces")
	p, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	return p
}

func TestStorageProvider_PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	payload := []byte("zk-object-fabric conformance payload")
	res, err := p.PutPiece(ctx, "piece-001", bytes.NewReader(payload), providers.PutOptions{
		ContentLength: int64(len(payload)),
		ContentType:   "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if res.SizeBytes != int64(len(payload)) {
		t.Fatalf("PutPiece size = %d, want %d", res.SizeBytes, len(payload))
	}

	rc, err := p.GetPiece(ctx, "piece-001", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read piece: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, payload)
	}
}

func TestStorageProvider_HeadPiece(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	payload := []byte("head-me")
	if _, err := p.PutPiece(ctx, "piece-head", bytes.NewReader(payload), providers.PutOptions{
		ContentType:  "text/plain",
		StorageClass: "standard",
		Metadata:     map[string]string{"origin": "test"},
	}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}

	md, err := p.HeadPiece(ctx, "piece-head")
	if err != nil {
		t.Fatalf("HeadPiece: %v", err)
	}
	if md.SizeBytes != int64(len(payload)) {
		t.Fatalf("HeadPiece size = %d, want %d", md.SizeBytes, len(payload))
	}
	if md.ContentType != "text/plain" {
		t.Fatalf("HeadPiece ContentType = %q, want %q", md.ContentType, "text/plain")
	}
	if md.Metadata["origin"] != "test" {
		t.Fatalf("HeadPiece Metadata[origin] = %q, want %q", md.Metadata["origin"], "test")
	}
}

func TestStorageProvider_DeletePiece(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	if _, err := p.PutPiece(ctx, "piece-delete", bytes.NewReader([]byte("bye")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if err := p.DeletePiece(ctx, "piece-delete"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if _, err := p.HeadPiece(ctx, "piece-delete"); err == nil {
		t.Fatal("HeadPiece after delete: want error, got nil")
	}
	if err := p.DeletePiece(ctx, "piece-delete"); err == nil {
		t.Fatal("DeletePiece of missing piece: want error, got nil")
	}
}

func TestStorageProvider_ListPieces(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	ids := []string{"alpha-1", "alpha-2", "beta-1"}
	for _, id := range ids {
		if _, err := p.PutPiece(ctx, id, bytes.NewReader([]byte(id)), providers.PutOptions{}); err != nil {
			t.Fatalf("PutPiece %q: %v", id, err)
		}
	}

	lr, err := p.ListPieces(ctx, "alpha-", "")
	if err != nil {
		t.Fatalf("ListPieces: %v", err)
	}
	if len(lr.Pieces) != 2 {
		t.Fatalf("ListPieces alpha-: got %d pieces, want 2", len(lr.Pieces))
	}
	for _, pm := range lr.Pieces {
		if !strings.HasPrefix(pm.PieceID, "alpha-") {
			t.Fatalf("ListPieces returned out-of-prefix piece %q", pm.PieceID)
		}
	}
}

func TestStorageProvider_ByteRange(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	payload := []byte("0123456789")
	if _, err := p.PutPiece(ctx, "piece-range", bytes.NewReader(payload), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}

	cases := []struct {
		name  string
		start int64
		end   int64
		want  string
	}{
		{"prefix", 0, 3, "0123"},
		{"middle", 4, 6, "456"},
		{"tail-open", 7, -1, "789"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rc, err := p.GetPiece(ctx, "piece-range", &providers.ByteRange{Start: tc.start, End: tc.end})
			if err != nil {
				t.Fatalf("GetPiece: %v", err)
			}
			defer rc.Close()
			got, err := io.ReadAll(rc)
			if err != nil {
				t.Fatalf("read range: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("range [%d,%d] = %q, want %q", tc.start, tc.end, got, tc.want)
			}
		})
	}
}

func TestStorageProvider_IfNoneMatch(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	if _, err := p.PutPiece(ctx, "piece-once", bytes.NewReader([]byte("x")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece first: %v", err)
	}
	_, err := p.PutPiece(ctx, "piece-once", bytes.NewReader([]byte("x")), providers.PutOptions{IfNoneMatch: true})
	if err == nil {
		t.Fatal("PutPiece IfNoneMatch on existing piece: want error, got nil")
	}
}

func TestStorageProvider_MissingPieceErrors(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	if _, err := p.GetPiece(ctx, "does-not-exist", nil); err == nil {
		t.Fatal("GetPiece missing: want error, got nil")
	} else if errors.Is(err, io.EOF) {
		t.Fatal("GetPiece missing returned EOF, want a not-found error")
	}
	if _, err := p.HeadPiece(ctx, "does-not-exist"); err == nil {
		t.Fatal("HeadPiece missing: want error, got nil")
	}
}

func TestStorageProvider_RejectsUnsafePieceIDs(t *testing.T) {
	ctx := context.Background()
	p := newProvider(t)

	unsafe := []string{
		"../escape",
		"..",
		".",
		".hidden",
		"nested/id",
		`back\slash`,
		"",
	}
	for _, id := range unsafe {
		t.Run("put/"+id, func(t *testing.T) {
			if _, err := p.PutPiece(ctx, id, bytes.NewReader([]byte("x")), providers.PutOptions{}); err == nil {
				t.Fatalf("PutPiece(%q): want error, got nil", id)
			}
			if _, err := p.GetPiece(ctx, id, nil); err == nil {
				t.Fatalf("GetPiece(%q): want error, got nil", id)
			}
			if _, err := p.HeadPiece(ctx, id); err == nil {
				t.Fatalf("HeadPiece(%q): want error, got nil", id)
			}
			if err := p.DeletePiece(ctx, id); err == nil {
				t.Fatalf("DeletePiece(%q): want error, got nil", id)
			}
		})
	}
}

func TestStorageProvider_DescriptiveMethods(t *testing.T) {
	p := newProvider(t)
	caps := p.Capabilities()
	if !caps.SupportsRangeReads {
		t.Fatal("Capabilities.SupportsRangeReads: want true")
	}
	labels := p.PlacementLabels()
	if labels.Provider == "" {
		t.Fatal("PlacementLabels.Provider: want non-empty")
	}
	_ = p.CostModel()
}
