// Package local_fs_dev implements the StorageProvider interface on
// top of a local filesystem. It exists for developer loopback and for
// the StorageProvider conformance test suite, which must be runnable
// without cloud credentials.
//
// Pieces are stored as files under a root directory. Metadata is kept
// in a sidecar JSON file next to each piece so HeadPiece does not need
// to re-stat or re-hash the payload.
package local_fs_dev

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Provider is a filesystem-backed StorageProvider used for dev and
// conformance tests.
type Provider struct {
	root string
	mu   sync.Mutex
}

// New returns a Provider rooted at root. The directory is created if
// it does not exist.
func New(root string) (*Provider, error) {
	if root == "" {
		return nil, errors.New("local_fs_dev: root path is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("local_fs_dev: create root %q: %w", root, err)
	}
	return &Provider{root: root}, nil
}

// sidecar holds the metadata persisted next to each piece.
type sidecar struct {
	PieceID      string            `json:"piece_id"`
	SizeBytes    int64             `json:"size_bytes"`
	ETag         string            `json:"etag"`
	ContentType  string            `json:"content_type"`
	StorageClass string            `json:"storage_class"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

func (p *Provider) piecePath(pieceID string) string {
	return filepath.Join(p.root, pieceID+".bin")
}

func (p *Provider) metaPath(pieceID string) string {
	return filepath.Join(p.root, pieceID+".json")
}

// PutPiece writes r to disk at {root}/{pieceID}.bin and records a
// sidecar JSON next to it.
func (p *Provider) PutPiece(_ context.Context, pieceID string, r io.Reader, opts providers.PutOptions) (providers.PutResult, error) {
	if pieceID == "" {
		return providers.PutResult{}, errors.New("local_fs_dev: pieceID is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	dataPath := p.piecePath(pieceID)
	metaPath := p.metaPath(pieceID)

	if opts.IfNoneMatch {
		if _, err := os.Stat(dataPath); err == nil {
			return providers.PutResult{}, fmt.Errorf("local_fs_dev: piece %q already exists", pieceID)
		}
	}

	tmp, err := os.CreateTemp(p.root, ".tmp-*")
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: create temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	size, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: write piece %q: %w", pieceID, err)
	}
	if err := tmp.Close(); err != nil {
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: close piece %q: %w", pieceID, err)
	}
	if err := os.Rename(tmpName, dataPath); err != nil {
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: finalize piece %q: %w", pieceID, err)
	}

	sc := sidecar{
		PieceID:      pieceID,
		SizeBytes:    size,
		ETag:         fmt.Sprintf("W/\"%d\"", size),
		ContentType:  opts.ContentType,
		StorageClass: opts.StorageClass,
		Metadata:     opts.Metadata,
	}
	metaBytes, err := json.Marshal(sc)
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: marshal sidecar %q: %w", pieceID, err)
	}
	if err := os.WriteFile(metaPath, metaBytes, 0o644); err != nil {
		return providers.PutResult{}, fmt.Errorf("local_fs_dev: write sidecar %q: %w", pieceID, err)
	}

	return providers.PutResult{
		PieceID:   pieceID,
		ETag:      sc.ETag,
		SizeBytes: size,
		Backend:   "local_fs_dev",
		Locator:   "file://" + dataPath,
	}, nil
}

// GetPiece returns a ReadCloser for the piece, honouring byteRange.
func (p *Provider) GetPiece(_ context.Context, pieceID string, byteRange *providers.ByteRange) (io.ReadCloser, error) {
	f, err := os.Open(p.piecePath(pieceID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("local_fs_dev: piece %q not found", pieceID)
		}
		return nil, fmt.Errorf("local_fs_dev: open piece %q: %w", pieceID, err)
	}
	if byteRange == nil {
		return f, nil
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("local_fs_dev: stat piece %q: %w", pieceID, err)
	}
	size := info.Size()
	start := byteRange.Start
	end := byteRange.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if start < 0 || start > end {
		f.Close()
		return nil, fmt.Errorf("local_fs_dev: invalid byte range [%d,%d] for piece %q size %d", byteRange.Start, byteRange.End, pieceID, size)
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		f.Close()
		return nil, fmt.Errorf("local_fs_dev: seek piece %q: %w", pieceID, err)
	}
	return &limitedReadCloser{
		ReadCloser: f,
		Reader:     io.LimitReader(f, end-start+1),
	}, nil
}

type limitedReadCloser struct {
	io.ReadCloser
	Reader io.Reader
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.Reader.Read(p) }

// HeadPiece returns the sidecar metadata for a piece.
func (p *Provider) HeadPiece(_ context.Context, pieceID string) (providers.PieceMetadata, error) {
	sc, err := p.readSidecar(pieceID)
	if err != nil {
		return providers.PieceMetadata{}, err
	}
	return providers.PieceMetadata{
		PieceID:      sc.PieceID,
		SizeBytes:    sc.SizeBytes,
		ETag:         sc.ETag,
		ContentType:  sc.ContentType,
		StorageClass: sc.StorageClass,
		Metadata:     sc.Metadata,
	}, nil
}

// DeletePiece removes the piece file and its sidecar. It is idempotent
// in the sense that missing files are not treated as errors, but the
// first Delete of a non-existent piece returns an error so callers can
// distinguish delete-of-missing from delete-of-existing.
func (p *Provider) DeletePiece(_ context.Context, pieceID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dataPath := p.piecePath(pieceID)
	if _, err := os.Stat(dataPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("local_fs_dev: piece %q not found", pieceID)
		}
		return fmt.Errorf("local_fs_dev: stat piece %q: %w", pieceID, err)
	}
	if err := os.Remove(dataPath); err != nil {
		return fmt.Errorf("local_fs_dev: remove piece %q: %w", pieceID, err)
	}
	if err := os.Remove(p.metaPath(pieceID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("local_fs_dev: remove sidecar %q: %w", pieceID, err)
	}
	return nil
}

// ListPieces returns pieces whose IDs start with prefix, in sorted
// order. cursor is the last pieceID seen; use "" for the first page.
func (p *Provider) ListPieces(_ context.Context, prefix, cursor string) (providers.ListResult, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return providers.ListResult{}, fmt.Errorf("local_fs_dev: readdir %q: %w", p.root, err)
	}

	var ids []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".bin") {
			continue
		}
		id := strings.TrimSuffix(name, ".bin")
		if prefix != "" && !strings.HasPrefix(id, prefix) {
			continue
		}
		if cursor != "" && id <= cursor {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)

	const pageLimit = 1000
	var next string
	if len(ids) > pageLimit {
		next = ids[pageLimit-1]
		ids = ids[:pageLimit]
	}

	out := make([]providers.PieceMetadata, 0, len(ids))
	for _, id := range ids {
		sc, err := p.readSidecar(id)
		if err != nil {
			continue
		}
		out = append(out, providers.PieceMetadata{
			PieceID:      sc.PieceID,
			SizeBytes:    sc.SizeBytes,
			ETag:         sc.ETag,
			ContentType:  sc.ContentType,
			StorageClass: sc.StorageClass,
			Metadata:     sc.Metadata,
		})
	}
	return providers.ListResult{Pieces: out, NextCursor: next}, nil
}

// Capabilities reports what local_fs_dev can do. It supports range
// reads and If-None-Match, but not multipart or server-side copy.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{
		SupportsRangeReads:     true,
		SupportsMultipart:      false,
		SupportsIfNoneMatch:    true,
		SupportsServerSideCopy: false,
		MaxObjectSizeBytes:     1 << 40,
		MinStorageDurationDays: 0,
	}
}

// CostModel returns a zero-cost model; this adapter is for dev only.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{}
}

// PlacementLabels tags this adapter as a local dev backend.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider: "local_fs_dev",
		Region:   "local",
		Country:  "XX",
		Tags: map[string]string{
			"class": "dev",
		},
	}
}

func (p *Provider) readSidecar(pieceID string) (sidecar, error) {
	data, err := os.ReadFile(p.metaPath(pieceID))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sidecar{}, fmt.Errorf("local_fs_dev: piece %q not found", pieceID)
		}
		return sidecar{}, fmt.Errorf("local_fs_dev: read sidecar %q: %w", pieceID, err)
	}
	var sc sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return sidecar{}, fmt.Errorf("local_fs_dev: parse sidecar %q: %w", pieceID, err)
	}
	return sc, nil
}

// Ensure Provider satisfies the interface at compile time.
var _ providers.StorageProvider = (*Provider)(nil)
