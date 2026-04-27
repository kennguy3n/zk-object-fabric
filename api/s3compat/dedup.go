// Intra-tenant deduplication helpers shared by the single-piece
// PUT, the multipart CompleteMultipartUpload, and the DELETE path.
//
// See docs/PROPOSAL.md §3.14 for the design. Two patterns are
// supported:
//
//   - Pattern B (gateway convergent): when the tenant policy uses
//     "managed" or "public_distribution" encryption AND has dedup
//     enabled, the gateway reads plaintext, computes BLAKE3 of the
//     plaintext for the convergent DEK, encrypts with deterministic
//     nonces, and dedups on BLAKE3(ciphertext).
//
//   - Pattern C (client-side convergent): when the tenant policy
//     uses "client_side" encryption AND has dedup enabled, the
//     client is expected to send convergent ciphertext. The gateway
//     dedups on BLAKE3(ciphertext_bytes) without ever touching
//     plaintext.
//
// Cross-tenant dedup is permanently excluded: every ContentIndex
// lookup is scoped to tenant_id.
package s3compat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// dedupEnabled reports whether the gateway should run the dedup
// flow for an object resolved by ResolveBackend. It is the single
// guard the PUT and DELETE paths consult so the policy invariant
// (need both store + per-object policy + Enabled flag) lives in
// one place.
func (h *Handler) dedupEnabled(policy metadata.PlacementPolicy) bool {
	if h.cfg.ContentIndex == nil {
		return false
	}
	if policy.DedupPolicy == nil || !policy.DedupPolicy.Enabled {
		return false
	}
	return true
}

// dedupResult captures the bytes the PUT path will write to the
// backend (or skip writing, if Hit) and the metadata it must record
// on the manifest.
type dedupResult struct {
	// Hit is true when ContentIndex already had an entry for
	// (tenant, contentHash) and the refcount has been bumped by
	// this call. The caller MUST skip the backend PutPiece and
	// reuse Existing.PieceID + Existing.Backend on the manifest.
	Hit bool

	// Existing is populated only when Hit == true. Backend
	// names the provider that owns the piece (which may be
	// different from the placement decision).
	Existing *content_index.ContentIndexEntry

	// ContentHash is the hex-encoded BLAKE3 hash recorded on
	// the manifest. For Pattern B it is BLAKE3(ciphertext); for
	// Pattern C it is BLAKE3(ciphertext_bytes). (Plaintext-hash
	// is held only transiently, used to derive the convergent
	// DEK.)
	ContentHash string

	// CiphertextBytes is the bytes the caller writes to the
	// backend on a miss. It is empty when Hit == true. The
	// caller must NOT reuse this buffer after the manifest is
	// written; the gateway holds dedup objects in memory only
	// for the duration of a single PUT.
	CiphertextBytes []byte

	// PlaintextSize is the original (pre-encryption) byte count.
	// Recorded on the manifest as ObjectSize for managed /
	// public_distribution modes; clients reading a deduped
	// object decrypt the same plaintext size every time.
	PlaintextSize int64

	// Encryption is the EncryptionConfig the manifest must
	// record. For Pattern B it includes the wrapped convergent
	// DEK; for Pattern C it is the algorithm declared by the
	// client header.
	Encryption metadata.EncryptionConfig
}

// blake3Hex computes BLAKE3-256(b) and returns the hex-encoded
// digest. Hex (rather than base64) keeps the value diff-friendly
// in logs and SQL output.
func blake3Hex(b []byte) string {
	sum := blake3.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

// ContentHashAlgo is the algorithm tag used at the start of every
// content_hash recorded on a manifest or in the content_index.
// Versioned so a future hash rotation does not collide with
// existing entries.
const ContentHashAlgo = "blake3"

// formatContentHash prefixes the hex digest with the algorithm
// tag so every persisted hash carries its own self-description.
// Manifests that predate Phase 3.5 have an empty ContentHash; ones
// written after carry the prefix.
func formatContentHash(hexDigest string) string {
	return ContentHashAlgo + ":" + hexDigest
}

// prepareDedupedPut runs the ContentIndex lookup/register flow and
// returns a dedupResult the caller (single-piece PUT, multipart
// complete) uses to decide whether to issue a backend write.
//
// encMode selects the pattern:
//
//   - "managed" / "public_distribution" → Pattern B (gateway
//     convergent). plaintext must hold the full plaintext bytes;
//     the gateway will encrypt them with a convergent DEK.
//
//   - "client_side" → Pattern C (client-side convergent). plaintext
//     must hold the ciphertext bytes the client just uploaded; the
//     gateway will not encrypt anything.
//
// On success the returned dedupResult.Hit reflects whether the
// caller should skip the backend write. The caller is responsible
// for emitting billing events and writing the manifest.
func (h *Handler) prepareDedupedPut(ctx context.Context, tenantID, encMode string, body []byte) (*dedupResult, error) {
	switch encMode {
	case string(encryption.ManagedEncrypted), string(encryption.PublicDistribution):
		return h.prepareDedupedPutPatternB(ctx, tenantID, encMode, body)
	case string(encryption.StrictZK):
		return h.prepareDedupedPutPatternC(ctx, tenantID, body)
	default:
		// Legacy / unencrypted writes never participate in
		// dedup. The caller's "is dedup enabled" guard should
		// have already short-circuited; surface a programmer
		// error if we reach here.
		return nil, fmt.Errorf("s3compat: dedup not supported for encryption mode %q", encMode)
	}
}

// prepareDedupedPutPatternB runs Pattern B (gateway convergent).
//
// Steps:
//  1. Hash plaintext with BLAKE3 to compute the convergent DEK
//     input.
//  2. Derive convergent DEK = HKDF(content_hash, salt=tenantID).
//  3. Encrypt with deterministic per-chunk nonces.
//  4. Hash ciphertext to get the ContentIndex key.
//  5. Lookup; on hit, IncrementRef and return Hit=true.
//  6. On miss, return CiphertextBytes for the caller to write,
//     then the caller calls registerDedupedPiece after a
//     successful backend write.
func (h *Handler) prepareDedupedPutPatternB(ctx context.Context, tenantID, encMode string, plaintext []byte) (*dedupResult, error) {
	if h.cfg.Encryption == nil {
		return nil, errors.New("s3compat: gateway encryption is not configured")
	}
	plaintextHash := blake3.Sum256(plaintext)
	dek, err := client_sdk.DeriveConvergentDEK(plaintextHash[:], tenantID)
	if err != nil {
		return nil, fmt.Errorf("s3compat: derive convergent dek: %w", err)
	}
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{
		ConvergentNonce: true,
	})
	if err != nil {
		return nil, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		return nil, fmt.Errorf("s3compat: read ciphertext: %w", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, fmt.Errorf("s3compat: wrap dek: %w", err)
	}
	contentHash := formatContentHash(blake3Hex(ciphertext))
	encCfg := metadata.EncryptionConfig{
		Mode:          encMode,
		Algorithm:     client_sdk.ContentAlgorithm,
		KeyID:         wrapped.KeyID,
		WrappedDEK:    wrapped.WrappedKey,
		WrapAlgorithm: wrapped.WrapAlgorithm,
	}

	existing, err := h.cfg.ContentIndex.Lookup(ctx, tenantID, contentHash)
	if err != nil && !errors.Is(err, content_index.ErrNotFound) {
		return nil, fmt.Errorf("s3compat: content_index lookup: %w", err)
	}
	if existing != nil {
		if err := h.cfg.ContentIndex.IncrementRef(ctx, tenantID, contentHash); err != nil {
			return nil, fmt.Errorf("s3compat: content_index increment: %w", err)
		}
		return &dedupResult{
			Hit:           true,
			Existing:      existing,
			ContentHash:   contentHash,
			PlaintextSize: int64(len(plaintext)),
			Encryption:    encCfg,
		}, nil
	}
	return &dedupResult{
		Hit:             false,
		ContentHash:     contentHash,
		CiphertextBytes: ciphertext,
		PlaintextSize:   int64(len(plaintext)),
		Encryption:      encCfg,
	}, nil
}

// prepareDedupedPutPatternC runs Pattern C (client-side convergent).
//
// The gateway cannot derive a convergent DEK because it never sees
// plaintext. It hashes the ciphertext the client uploaded and uses
// that as the dedup key. Tenants are responsible for configuring
// their client SDK to emit byte-deterministic ciphertext (e.g. by
// setting ConvergentNonce=true with a tenant-shared, content-derived
// DEK) — clients that send unique random nonces per upload will
// never hit the dedup path.
func (h *Handler) prepareDedupedPutPatternC(ctx context.Context, tenantID string, ciphertext []byte) (*dedupResult, error) {
	contentHash := formatContentHash(blake3Hex(ciphertext))
	encCfg := metadata.EncryptionConfig{
		Mode:      string(encryption.StrictZK),
		Algorithm: client_sdk.ContentAlgorithm,
	}

	existing, err := h.cfg.ContentIndex.Lookup(ctx, tenantID, contentHash)
	if err != nil && !errors.Is(err, content_index.ErrNotFound) {
		return nil, fmt.Errorf("s3compat: content_index lookup: %w", err)
	}
	if existing != nil {
		if err := h.cfg.ContentIndex.IncrementRef(ctx, tenantID, contentHash); err != nil {
			return nil, fmt.Errorf("s3compat: content_index increment: %w", err)
		}
		return &dedupResult{
			Hit:           true,
			Existing:      existing,
			ContentHash:   contentHash,
			PlaintextSize: int64(len(ciphertext)),
			Encryption:    encCfg,
		}, nil
	}
	return &dedupResult{
		Hit:             false,
		ContentHash:     contentHash,
		CiphertextBytes: ciphertext,
		PlaintextSize:   int64(len(ciphertext)),
		Encryption:      encCfg,
	}, nil
}

// registerDedupedPiece records a freshly-written piece in the
// content index. It is the miss-path counterpart to the lookup
// done in prepareDedupedPut: the caller has just successfully
// PutPiece'd the piece, and now publishes it as the canonical
// ciphertext for (tenantID, contentHash).
//
// If a concurrent uploader registered the same content first
// (ErrAlreadyExists), the function falls back to IncrementRef and
// reports Hit=true via the returned bool. The caller MUST then
// undo its backend write (provider.DeletePiece) to avoid
// orphaning the duplicate piece.
func (h *Handler) registerDedupedPiece(ctx context.Context, entry content_index.ContentIndexEntry) (raceLost bool, err error) {
	regErr := h.cfg.ContentIndex.Register(ctx, entry)
	if regErr == nil {
		return false, nil
	}
	if !errors.Is(regErr, content_index.ErrAlreadyExists) {
		return false, regErr
	}
	if err := h.cfg.ContentIndex.IncrementRef(ctx, entry.TenantID, entry.ContentHash); err != nil {
		return true, fmt.Errorf("s3compat: content_index race recovery: %w", err)
	}
	return true, nil
}

// putDeduped is the single-piece PUT path when intra-tenant dedup
// is enabled. It buffers the request body, runs the
// pattern-specific lookup/register flow, and writes a manifest
// that either references an existing deduped piece (Hit) or a
// fresh one this PUT just wrote.
//
// On a hit the manifest's Pieces[0] points at the existing piece
// (which may live on a different backend than the one
// ResolveBackend chose for this request — the canonical copy
// wins) and no backend write occurs.
//
// On a miss the gateway does a single PutPiece, registers the
// entry, and persists the manifest. If a concurrent uploader
// registered the same content first the gateway DeletePiece's the
// duplicate before persisting the manifest, then redirects the
// manifest at the canonical piece via a follow-up Lookup.
func (h *Handler) putDeduped(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, bucket, key, backendName string,
	provider providers.StorageProvider,
	policy metadata.PlacementPolicy,
) {
	encMode := policy.EncryptionMode

	// client_side mode requires the X-Amz-Meta-Zk-Encryption
	// header up-front so a tenant on Strict ZK who forgot it
	// fails before we read the body. The non-dedup path enforces
	// the same guard inside prepareSinglePieceEncryption.
	if encMode == string(encryption.StrictZK) {
		if r.Header.Get("X-Amz-Meta-Zk-Encryption") == "" {
			writeError(w, http.StatusForbidden, "EncryptionRequired",
				"tenant policy requires client_side encryption; set X-Amz-Meta-Zk-Encryption header", r.URL.Path)
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "read body: "+err.Error(), r.URL.Path)
		return
	}

	res, err := h.prepareDedupedPut(r.Context(), tenantID, encMode, body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "DedupFailed", err.Error(), r.URL.Path)
		return
	}

	var pieceID, pieceBackend, pieceHash, pieceLocator string
	var sizeOnWire int64
	// dedupHitEffective is true when this PUT did not actually
	// store new bytes on the backend — either because the
	// content_index Lookup hit (res.Hit) OR because the Register
	// race was lost and the manifest was redirected to the
	// canonical piece. The billing path uses it to suppress
	// StorageBytesSeconds for the race-lost case (the canonical
	// piece was already counted by the original uploader).
	dedupHitEffective := false
	if res.Hit {
		dedupHitEffective = true
		pieceID = res.Existing.PieceID
		pieceBackend = res.Existing.Backend
		// Reuse the canonical ETag the first uploader's PUT
		// response returned so dedup-hit clients get the same
		// ETag a non-dedup PUT of the same content would have.
		// Locator is left empty: the manifest's PieceID +
		// Backend are sufficient to round-trip GETs through
		// the provider, and the locator is opaque to clients.
		pieceHash = res.Existing.ETag
		sizeOnWire = res.Existing.SizeBytes
		// Emit dedup-hit billing dimensions so operators can
		// reconcile bytes-saved vs. bytes-written.
		h.emit(tenantID, bucket, billing.DedupHits, 1)
		if res.Existing.SizeBytes > 0 {
			h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(res.Existing.SizeBytes))
		}
	} else {
		pieceID = newPieceID(tenantID, bucket, key, h.cfg.Now())
		putRes, perr := provider.PutPiece(r.Context(), pieceID, bytes.NewReader(res.CiphertextBytes), providers.PutOptions{
			ContentLength: int64(len(res.CiphertextBytes)),
			ContentType:   r.Header.Get("Content-Type"),
		})
		if perr != nil {
			writeError(w, http.StatusBadGateway, "BackendPutFailed", perr.Error(), r.URL.Path)
			return
		}
		pieceID = putRes.PieceID
		pieceBackend = backendName
		pieceHash = putRes.ETag
		pieceLocator = putRes.Locator
		sizeOnWire = putRes.SizeBytes

		// Register the freshly written piece. A concurrent
		// uploader of the same content may have raced us; in
		// that case Register returns ErrAlreadyExists and we
		// fall back to IncrementRef on the canonical entry.
		raceLost, regErr := h.registerDedupedPiece(r.Context(), content_index.ContentIndexEntry{
			TenantID:    tenantID,
			ContentHash: res.ContentHash,
			PieceID:     pieceID,
			Backend:     pieceBackend,
			SizeBytes:   sizeOnWire,
			ETag:        pieceHash,
		})
		if regErr != nil {
			// Best-effort cleanup of the orphaned piece so we
			// don't leave billable storage behind.
			_ = provider.DeletePiece(r.Context(), pieceID)
			writeError(w, http.StatusInternalServerError, "ContentIndexRegisterFailed", regErr.Error(), r.URL.Path)
			return
		}
		if raceLost {
			// Drop the duplicate piece and redirect the
			// manifest at the canonical copy.
			_ = provider.DeletePiece(r.Context(), pieceID)
			canonical, lookupErr := h.cfg.ContentIndex.Lookup(r.Context(), tenantID, res.ContentHash)
			if lookupErr != nil {
				// Roll back the IncrementRef that
				// registerDedupedPiece already performed:
				// no manifest will be written for this PUT,
				// so leaving the bump in place would
				// permanently inflate the canonical entry's
				// refcount and prevent eventual cleanup.
				_, _ = h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, res.ContentHash)
				writeError(w, http.StatusInternalServerError, "ContentIndexLookupFailed", lookupErr.Error(), r.URL.Path)
				return
			}
			pieceID = canonical.PieceID
			pieceBackend = canonical.Backend
			pieceHash = canonical.ETag
			pieceLocator = ""
			sizeOnWire = canonical.SizeBytes
			dedupHitEffective = true
			h.emit(tenantID, bucket, billing.DedupHits, 1)
			if canonical.SizeBytes > 0 {
				h.emit(tenantID, bucket, billing.DedupBytesSaved, uint64(canonical.SizeBytes))
			}
		}
	}

	// ObjectSize follows the gateway-encrypted convention: for
	// managed / public_distribution it is the plaintext size so
	// clients see what they uploaded, for client_side it is the
	// ciphertext size (the gateway never sees plaintext).
	objectSize := sizeOnWire
	if IsGatewayEncrypted(encMode) {
		objectSize = res.PlaintextSize
	}

	versionID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKey:       key,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       versionID,
		ObjectSize:      objectSize,
		ChunkSize:       sizeOnWire,
		ContentHash:     res.ContentHash,
		Encryption:      res.Encryption,
		PlacementPolicy: policy,
		Pieces: []metadata.Piece{{
			PieceID:   pieceID,
			Backend:   pieceBackend,
			Locator:   pieceLocator,
			Hash:      pieceHash,
			SizeBytes: sizeOnWire,
			State:     "active",
		}},
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: pieceBackend,
		},
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		// On manifest failure roll back the refcount so the
		// content index is consistent with what is reachable
		// via manifests. We do NOT delete the piece here even
		// for a fresh write: the refcount drop handles it
		// uniformly with the hit path, and on the off chance
		// another concurrent uploader Lookup'd it between our
		// Register and this rollback we would corrupt their
		// upload by deleting the piece.
		if _, derr := h.cfg.ContentIndex.DecrementRef(r.Context(), tenantID, res.ContentHash); derr != nil {
			// Best-effort: log via the standard error path.
			_ = derr
		}
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, bucket, billing.PutRequests, 1)
	// Only emit StorageBytesSeconds when this PUT actually wrote
	// new bytes to the backend. dedupHitEffective covers both the
	// straight Lookup-hit and the race-lost-Register path; without
	// the second guard a race-lost PUT would double-count storage
	// (the canonical piece was billed when the original uploader
	// stored it).
	if !dedupHitEffective && sizeOnWire > 0 {
		h.emit(tenantID, bucket, billing.StorageBytesSeconds, uint64(sizeOnWire))
	}

	var country string
	if prov, ok := h.cfg.Providers[pieceBackend]; ok {
		country = prov.PlacementLabels().Country
	}
	h.audit(r, "PUT", tenantID, bucket, key, pieceID, pieceBackend, country)

	if pieceHash != "" {
		w.Header().Set("ETag", quote(pieceHash))
	}
	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
}

// hashAssembledPieces streams the bytes of every piece in order,
// reading them back from their respective backends, and returns the
// canonical content_hash. The hash is computed over the concatenated
// piece bytes — i.e. the on-wire representation of the assembled
// object as the GET path would deliver it.
//
// The function buffers the pieces into a single BLAKE3 hasher so the
// memory cost is bounded by the streaming buffer rather than the
// full object size. Errors propagate verbatim so the caller can
// distinguish provider failures from bookkeeping mistakes.
func (h *Handler) hashAssembledPieces(ctx context.Context, pieces []metadata.Piece) (string, error) {
	hasher := blake3.New()
	for _, piece := range pieces {
		provider, ok := h.cfg.Providers[piece.Backend]
		if !ok {
			return "", fmt.Errorf("s3compat: hash assembled pieces: backend %q not registered", piece.Backend)
		}
		rc, err := provider.GetPiece(ctx, piece.PieceID, nil)
		if err != nil {
			return "", fmt.Errorf("s3compat: hash assembled pieces: get piece %s: %w", piece.PieceID, err)
		}
		_, copyErr := io.Copy(hasher, rc)
		_ = rc.Close()
		if copyErr != nil {
			return "", fmt.Errorf("s3compat: hash assembled pieces: copy piece %s: %w", piece.PieceID, copyErr)
		}
	}
	sum := hasher.Sum(nil)
	return formatContentHash(fmt.Sprintf("%x", sum)), nil
}
