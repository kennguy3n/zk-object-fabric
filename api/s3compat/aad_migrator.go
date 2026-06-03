// aad_migrator.go: background worker that upgrades legacy
// gateway-encrypted objects (EncryptionConfig.AADVersion == "") to the
// modern v1 per-chunk AAD scheme in place.
//
// Phase A2a bound every NEW managed write's per-chunk AEAD tag to the
// canonical object identity tenant_id|bucket|object_key_hash|
// version_id (see encryption_pipeline.go), but objects written before
// that change carry AADVersion == "" and were sealed with AAD = nil.
// The GET path keeps opening them with nil AAD, so they remain
// readable — but they do not benefit from the identity binding that
// makes a relocated/confused ciphertext fail closed. This worker walks
// every manifest and re-encrypts the eligible legacy ones so the whole
// fleet converges on v1.
//
// In-place re-encryption preserves the object's identity: the
// manifest's version_id is unchanged, so the v1 AAD is bound to the
// SAME identity the GET path already reconstructs. Only the backend
// piece bytes (new DEK, new ciphertext), the piece id, and the
// recorded EncryptionConfig change. The worker writes the new piece
// under a fresh piece id, atomically switches the manifest to point at
// it, then deletes the old piece.
//
// Eligibility is deliberately narrow so the worker only ever touches
// objects it can re-seal correctly and cheaply:
//
//   - gateway-encrypted (managed / public_distribution): client_side
//     objects hold customer-side DEKs the gateway cannot unwrap, and
//     "" / legacy-unencrypted objects have no ciphertext to re-seal.
//   - AADVersion == "": already-v1 objects are done.
//   - ContentHash == "": convergent / dedup objects are sealed with a
//     convergent nonce, which the SDK makes mutually exclusive with
//     ChunkAAD — they cannot carry a v1 binding and stay "" by design.
//   - single-piece: erasure-coded and multipart manifests hold many
//     pieces; re-encrypting them is a separate, larger workstream and
//     is left as a documented follow-up. The worker counts them as
//     skipped rather than touching a subset of their pieces.
//   - within the in-memory ceiling: re-encryption buffers the full
//     ciphertext, plaintext, and new ciphertext at once (~3x), exactly
//     like copyReencrypt, so objects above MaxInMemoryObjectBytes are
//     skipped (and logged) rather than risking an OOM.
package s3compat

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// MigratorLogger is the minimal logging surface the worker uses.
// *log.Logger satisfies it; tests pass a no-op.
type MigratorLogger interface {
	Printf(format string, args ...any)
}

// AADMigratorConfig configures the background AAD v1 migration sweep.
//
// Operator note — run a SINGLE instance. The worker has no
// distributed lock or compare-and-swap on the manifest: it relies on
// processing each object once, sequentially. Running concurrent
// sweeps across a multi-node fleet is safe for data integrity (both
// nodes re-encrypt the same plaintext under the same preserved
// identity, so whichever manifest Put wins yields a valid, decryptable
// v1 object) but can leak a backend piece — the losing node's freshly
// written piece becomes an orphan. Orphans are logged and never
// corrupt the object; they are independently reclaimable. Because this
// is a one-time, opt-in fleet upgrade, schedule it on one node during
// a low-traffic window rather than on every gateway.
type AADMigratorConfig struct {
	// Interval is the cadence between full sweeps. A value <= 0
	// disables the worker entirely (Run returns immediately); the
	// gateway only starts it when an operator opts in via config.
	Interval time.Duration

	// PageSize is the number of manifests fetched per ScanManifests
	// call. Zero defers to the store default (1000).
	PageSize int

	// PerObjectDelay throttles re-encryption: the worker sleeps for
	// this duration after each object it actually migrates so a
	// large fleet upgrade does not saturate the backends or the
	// gateway's CPU. Zero means no delay. Skipped (ineligible)
	// objects are not throttled.
	PerObjectDelay time.Duration

	// Logger receives one-line operator-facing notes about each
	// sweep. Optional; defaults to log.Default().
	Logger MigratorLogger
}

// AADMigrator re-encrypts legacy gateway-encrypted objects to AAD v1.
// It reuses the Handler's encrypt/decrypt pipeline and provider
// registry so the seal it produces is byte-for-byte the one the live
// PUT path would have produced for a v1 write.
type AADMigrator struct {
	h   *Handler
	cfg AADMigratorConfig
}

// NewAADMigrator validates cfg and returns a worker bound to h. h must
// have a manifest store and provider registry wired, which the gateway
// always does when this worker is enabled.
func NewAADMigrator(h *Handler, cfg AADMigratorConfig) (*AADMigrator, error) {
	if h == nil {
		return nil, errors.New("aad_migrator: handler is required")
	}
	if h.cfg.Manifests == nil {
		return nil, errors.New("aad_migrator: handler has no manifest store")
	}
	if h.cfg.Encryption == nil {
		return nil, errors.New("aad_migrator: handler has no gateway encryption configured")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &AADMigrator{h: h, cfg: cfg}, nil
}

// MigrateStats summarises a single sweep pass.
type MigrateStats struct {
	Scanned        int
	Migrated       int
	SkippedNotMine int // not gateway-encrypted (client_side / legacy plaintext)
	SkippedAlready int // already AADVersion == "v1"
	SkippedDedup   int // convergent / dedup object, cannot bind AAD
	SkippedMulti   int // multi-piece (EC / multipart), follow-up workstream
	SkippedTooBig  int // above the in-memory re-encrypt ceiling
	Errors         int
}

// migrateOutcome classifies what Migrate did with one manifest.
type migrateOutcome int

const (
	outcomeMigrated migrateOutcome = iota
	outcomeNotMine
	outcomeAlready
	outcomeDedup
	outcomeMulti
	outcomeTooBig
)

// Run sweeps once immediately and then every cfg.Interval until ctx
// is cancelled. It is a no-op (returns nil immediately) when
// Interval <= 0 so wiring the worker in without configuring an
// interval is safe. Each sweep's stats are logged; sweep errors are
// logged and the loop continues so a transient store failure does not
// stop future passes.
func (m *AADMigrator) Run(ctx context.Context) error {
	if m.cfg.Interval <= 0 {
		m.cfg.Logger.Printf("aad_migrator: disabled (interval <= 0)")
		return nil
	}
	ticker := time.NewTicker(m.cfg.Interval)
	defer ticker.Stop()
	// Run one sweep immediately so enabling the worker makes progress
	// right away instead of sitting idle for a full interval (which is
	// typically hours). A fleet upgrade should start converging as
	// soon as an operator opts in; subsequent sweeps fire on the
	// ticker. The sweep itself is rate-limited per object via
	// PerObjectDelay, so an immediate start does not stampede backends.
	for {
		stats, err := m.Sweep(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			m.cfg.Logger.Printf("aad_migrator: sweep error: %v", err)
		} else {
			m.cfg.Logger.Printf("aad_migrator: sweep done: scanned=%d migrated=%d skipped(notmine=%d already=%d dedup=%d multi=%d toobig=%d) errors=%d",
				stats.Scanned, stats.Migrated, stats.SkippedNotMine, stats.SkippedAlready,
				stats.SkippedDedup, stats.SkippedMulti, stats.SkippedTooBig, stats.Errors)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Sweep performs one synchronous pass over every manifest the store
// reports, re-encrypting the eligible legacy ones. It returns
// aggregated counters; per-object errors are logged and counted in
// Stats.Errors but never abort the sweep (a poison object must not
// wedge the whole fleet upgrade). The sweep honours context
// cancellation between pages and between objects so it is promptly
// interruptible on shutdown.
func (m *AADMigrator) Sweep(ctx context.Context) (MigrateStats, error) {
	stats := MigrateStats{}
	cursor := ""
	for {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		page, err := m.h.cfg.Manifests.ScanManifests(ctx, cursor, m.cfg.PageSize)
		if err != nil {
			return stats, fmt.Errorf("aad_migrator: scan: %w", err)
		}
		for _, sm := range page.Manifests {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			stats.Scanned++
			outcome, merr := m.migrateOne(ctx, sm.Key, sm.Manifest)
			if merr != nil {
				stats.Errors++
				m.cfg.Logger.Printf("aad_migrator: tenant=%s bucket=%s version=%s migrate failed: %v",
					sm.Key.TenantID, sm.Key.Bucket, sm.Key.VersionID, merr)
				continue
			}
			switch outcome {
			case outcomeMigrated:
				stats.Migrated++
				if m.cfg.PerObjectDelay > 0 {
					select {
					case <-ctx.Done():
						return stats, ctx.Err()
					case <-time.After(m.cfg.PerObjectDelay):
					}
				}
			case outcomeNotMine:
				stats.SkippedNotMine++
			case outcomeAlready:
				stats.SkippedAlready++
			case outcomeDedup:
				stats.SkippedDedup++
			case outcomeMulti:
				stats.SkippedMulti++
			case outcomeTooBig:
				stats.SkippedTooBig++
			}
		}
		if page.NextCursor == "" {
			return stats, nil
		}
		cursor = page.NextCursor
	}
}

// eligibility classifies a manifest without touching any backend. It
// returns outcomeMigrated only when the object is a re-encryptable
// legacy object; every other return value is a skip reason. Keeping
// this pure makes the Sweep accounting and the unit tests trivial.
func eligibility(m *metadata.ObjectManifest) migrateOutcome {
	if !IsGatewayEncrypted(m.Encryption.Mode) {
		return outcomeNotMine
	}
	if m.Encryption.AADVersion == AADVersionV1 {
		return outcomeAlready
	}
	// Defensive: an unknown non-empty AADVersion is not something
	// this worker knows how to re-seal; treat it as already-handled
	// rather than clobbering it.
	if m.Encryption.AADVersion != "" {
		return outcomeAlready
	}
	if m.ContentHash != "" {
		return outcomeDedup
	}
	if len(m.Pieces) != 1 {
		return outcomeMulti
	}
	if m.ObjectSize > MaxInMemoryObjectBytes {
		return outcomeTooBig
	}
	return outcomeMigrated
}

// migrateOne re-encrypts a single manifest in place when it is an
// eligible legacy object, otherwise returns the skip reason. The key
// is the store key the manifest lives under; the re-Put uses it
// verbatim so the object's identity (version_id) is unchanged.
func (m *AADMigrator) migrateOne(ctx context.Context, key manifest_store.ManifestKey, man *metadata.ObjectManifest) (migrateOutcome, error) {
	outcome := eligibility(man)
	if outcome != outcomeMigrated {
		return outcome, nil
	}

	oldPiece := man.Pieces[0]
	provider, ok := m.h.cfg.Providers[oldPiece.Backend]
	if !ok {
		return 0, fmt.Errorf("backend %q not registered", oldPiece.Backend)
	}

	// The v1 binding must use the object's CURRENT identity so the
	// GET path — which rebuilds the AAD from the unchanged manifest
	// identity fields — reproduces the identical AAD. version_id is
	// preserved across the migration, so this is exactly
	// aadIdentityOf(man).
	id := aadIdentityOf(man)

	body, err := provider.GetPiece(ctx, oldPiece.PieceID, nil)
	if err != nil {
		return 0, fmt.Errorf("get source piece: %w", err)
	}
	// Bound the read one byte over the ceiling: the eligibility
	// check already rejected oversize objects by their recorded
	// ObjectSize, but a stale manifest size or a misbehaving backend
	// must not be able to pull an unbounded body into memory. This
	// mirrors copyReencrypt's defence-in-depth.
	ciphertext, rerr := io.ReadAll(io.LimitReader(body, MaxInMemoryObjectBytes+1))
	_ = body.Close()
	if rerr != nil {
		return 0, fmt.Errorf("read source piece: %w", rerr)
	}
	if int64(len(ciphertext)) > MaxInMemoryObjectBytes {
		return outcomeTooBig, nil
	}

	plaintext, derr := m.h.decryptFromStorage(ciphertext, man.Encryption, id)
	if derr != nil {
		return 0, fmt.Errorf("decrypt legacy ciphertext: %w", derr)
	}

	newCiphertext, wrapped, eerr := m.h.encryptForStorage(plaintext, id)
	// Scrub the recovered plaintext as soon as the SDK has consumed
	// it; defence-in-depth against a heap dump exposing cleartext
	// that only transited the gateway for re-encryption.
	clear(plaintext)
	if eerr != nil {
		return 0, fmt.Errorf("re-encrypt to v1: %w", eerr)
	}

	// Write the re-encrypted bytes under a NEW backend piece id so
	// the switch is non-destructive: the old piece stays readable
	// for any GET that loaded the pre-switch manifest until we
	// delete it below. The piece id is independent of the object's
	// version_id, which is unchanged.
	newPieceID := newPieceID(man.TenantID, man.Bucket, man.ObjectKey, m.h.cfg.Now())
	cipherHash := blake3.Sum256(newCiphertext)
	putRes, perr := provider.PutPiece(ctx, newPieceID, bytes.NewReader(newCiphertext), providers.PutOptions{
		ContentLength: int64(len(newCiphertext)),
	})
	if perr != nil {
		return 0, fmt.Errorf("put re-encrypted piece: %w", perr)
	}

	updated := cloneForMigrate(man)
	updated.Pieces[0] = metadata.Piece{
		PieceID:      newPieceID,
		Hash:         "blake3:" + hex.EncodeToString(cipherHash[:]),
		ProviderETag: putRes.ETag,
		Backend:      oldPiece.Backend,
		Locator:      putRes.Locator,
		State:        "active",
		SizeBytes:    putRes.SizeBytes,
	}
	// Preserve Mode and ManifestEncrypted; everything else describing
	// the seal comes from the fresh v1 re-encrypt above.
	updated.Encryption = metadata.EncryptionConfig{
		Mode:              man.Encryption.Mode,
		Algorithm:         client_sdk.ContentAlgorithm,
		KeyID:             wrapped.KeyID,
		WrappedDEK:        wrapped.WrappedKey,
		WrapAlgorithm:     wrapped.WrapAlgorithm,
		ManifestEncrypted: man.Encryption.ManifestEncrypted,
		AADVersion:        AADVersionV1,
	}

	// Atomic switch: a single Put under the same key flips the
	// manifest from the old (nil-AAD) piece to the new (v1) piece.
	// A GET either sees the whole old manifest or the whole new one.
	if err := m.h.cfg.Manifests.Put(ctx, key, updated); err != nil {
		// The new piece is now an orphan (no manifest references
		// it). Best-effort clean it up so a failed switch does not
		// leak storage; if the delete fails too there is nothing
		// more we can safely do here.
		if delErr := provider.DeletePiece(ctx, newPieceID); delErr != nil {
			m.cfg.Logger.Printf("aad_migrator: tenant=%s version=%s manifest switch failed and orphan piece %s could not be deleted: %v",
				man.TenantID, man.VersionID, newPieceID, delErr)
		}
		return 0, fmt.Errorf("switch manifest: %w", err)
	}

	// Reclaim the old piece. The manifest no longer references it, so
	// it is now an orphan; non-dedup pieces are not tracked by the
	// content_index orphan GC, so the worker must delete it itself.
	// This is best-effort and runs AFTER the switch: a GET that
	// loaded the pre-switch manifest may still be fetching the old
	// piece in the small window before this delete lands, which is
	// why the migration is intended to run as a rate-limited
	// background sweep during low-traffic windows. A delete failure
	// leaks one piece but never corrupts the (already-switched)
	// object, so it is logged, not fatal.
	if delErr := provider.DeletePiece(ctx, oldPiece.PieceID); delErr != nil {
		m.cfg.Logger.Printf("aad_migrator: tenant=%s version=%s migrated but old piece %s leaked (delete failed): %v",
			man.TenantID, man.VersionID, oldPiece.PieceID, delErr)
	}
	return outcomeMigrated, nil
}

// cloneForMigrate makes a defensive copy of the manifest before the
// worker mutates it. migrateOne mutates exactly two things on the
// returned copy:
//
//   - Pieces[0], so Pieces is copied into a fresh backing array here
//     (the shallow struct copy would otherwise share it with m); and
//   - Encryption, which is a value type reassigned wholesale (not
//     mutated field-by-field), so the assignment never reaches back
//     into m's slices.
//
// Every other field is left untouched and is therefore safe to share.
// This is deliberately narrower than the store's full cloneManifest:
// IMPORTANT — if future migration logic starts mutating any other
// field that has slice/map/pointer contents (e.g.
// PlacementPolicy.Residency), extend this clone (or switch to a deep
// clone) so the original manifest's backing arrays are not aliased.
func cloneForMigrate(m *metadata.ObjectManifest) *metadata.ObjectManifest {
	cp := *m
	cp.Pieces = append([]metadata.Piece(nil), m.Pieces...)
	return &cp
}
