// Package lazy_read_repair implements on-demand migration of a single
// piece when a GET request lands on a manifest whose MigrationState
// already names a new primary but whose piece still sits on the old
// backend. See docs/PROPOSAL.md §4.3 and migration/state.go.
//
// The middleware is wired into the gateway's resolve() path: when the
// primary provider returns a not-found (or any error), the middleware
// tries the secondary backend, verifies the ciphertext hash against
// the manifest piece, copies the bytes into the new primary, and
// updates the manifest piece locator to reflect the repair. The
// response is served from the fetched data so the caller's latency
// budget is spent once, not twice.
package lazy_read_repair

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// ErrRepairUnavailable is returned when the middleware cannot locate
// any backend that has the piece. The caller should surface the
// original not-found to the user.
var ErrRepairUnavailable = errors.New("lazy_read_repair: no backend has the piece")

// ReadRepair coordinates the lazy read-repair path. It holds the
// providers registry so it can pick primary/secondary by backend
// name, and the ManifestStore so it can persist piece locator
// updates.
type ReadRepair struct {
	Providers map[string]providers.StorageProvider
	Manifests manifest_store.ManifestStore

	// Logger is optional; errors on the slow path are best surfaced
	// through it so repair failures don't get lost.
	Logger *log.Logger
}

// New builds a ReadRepair given a provider registry and manifest
// store. Both are required.
func New(providers map[string]providers.StorageProvider, store manifest_store.ManifestStore) *ReadRepair {
	return &ReadRepair{Providers: providers, Manifests: store}
}

// RepairResult describes the outcome of a single repair attempt.
type RepairResult struct {
	// Body is the piece ciphertext (buffered in memory); callers
	// serve it to the client.
	Body []byte
	// RepairedBackend is the backend the piece was copied to.
	RepairedBackend string
	// SourceBackend is the backend the piece was read from.
	SourceBackend string
}

// Repair fetches piece from the secondary backend named on the
// manifest, verifies its ETag/hash, writes a copy to the primary
// backend named on manifest.MigrationState.PrimaryBackend, and
// updates the piece locator on the manifest.
//
// Repair does not serve the HTTP response itself — the caller is
// responsible for copying RepairResult.Body into the response body.
// That keeps the middleware decoupled from the gateway's writer
// path and preserves range-read behaviour (the caller re-applies the
// ByteRange after repair).
func (r *ReadRepair) Repair(ctx context.Context, key manifest_store.ManifestKey, manifest *metadata.ObjectManifest, pieceIdx int) (RepairResult, error) {
	if r.Providers == nil || r.Manifests == nil {
		return RepairResult{}, errors.New("lazy_read_repair: provider map and manifest store are required")
	}
	if pieceIdx < 0 || pieceIdx >= len(manifest.Pieces) {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: piece index %d out of range", pieceIdx)
	}
	piece := manifest.Pieces[pieceIdx]
	targetBackend := manifest.MigrationState.PrimaryBackend
	if targetBackend == "" {
		targetBackend = piece.Backend
	}

	// If the piece already sits on the new primary there is nothing
	// to repair.
	if piece.Backend == targetBackend {
		return RepairResult{}, ErrRepairUnavailable
	}

	source, ok := r.Providers[piece.Backend]
	if !ok {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: piece backend %q not registered", piece.Backend)
	}
	target, ok := r.Providers[targetBackend]
	if !ok {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: target backend %q not registered", targetBackend)
	}

	rc, err := source.GetPiece(ctx, piece.PieceID, nil)
	if err != nil {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: read piece from %s: %w", piece.Backend, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: drain piece: %w", err)
	}

	if err := verifyPiece(body, piece); err != nil {
		return RepairResult{}, err
	}

	putRes, err := target.PutPiece(ctx, piece.PieceID, bytes.NewReader(body), providers.PutOptions{ContentLength: int64(len(body))})
	if err != nil {
		return RepairResult{}, fmt.Errorf("lazy_read_repair: write piece to %s: %w", targetBackend, err)
	}

	manifest.Pieces[pieceIdx].Backend = targetBackend
	if putRes.Locator != "" {
		manifest.Pieces[pieceIdx].Locator = putRes.Locator
	}
	if err := r.Manifests.Put(ctx, key, manifest); err != nil {
		r.logf("lazy_read_repair: persist manifest update for %s: %v", piece.PieceID, err)
		// Roll the in-memory piece locator back so the caller still
		// sees the original backend; the next request will retry
		// the repair.
		manifest.Pieces[pieceIdx].Backend = piece.Backend
		manifest.Pieces[pieceIdx].Locator = piece.Locator
		return RepairResult{}, fmt.Errorf("lazy_read_repair: manifest update: %w", err)
	}

	return RepairResult{
		Body:            body,
		RepairedBackend: targetBackend,
		SourceBackend:   piece.Backend,
	}, nil
}

// verifyPiece checks body against piece.Hash. It accepts the two
// hash forms Phase 2 emits: raw SHA-256 hex (from client SDK
// BLAKE3-placeholder) and S3 ETag ("\"<hex>\"" quoting). Empty
// piece.Hash skips the check — legacy pieces created before hashes
// were recorded fall through unverified.
func verifyPiece(body []byte, piece metadata.Piece) error {
	if piece.Hash == "" {
		return nil
	}
	expected := piece.Hash
	if len(expected) >= 2 && expected[0] == '"' && expected[len(expected)-1] == '"' {
		expected = expected[1 : len(expected)-1]
	}
	sum := sha256.Sum256(body)
	if expected == hex.EncodeToString(sum[:]) {
		return nil
	}
	return fmt.Errorf("lazy_read_repair: piece %s hash mismatch: expected %q", piece.PieceID, expected)
}

func (r *ReadRepair) logf(format string, args ...any) {
	if r.Logger == nil {
		return
	}
	r.Logger.Printf(format, args...)
}
