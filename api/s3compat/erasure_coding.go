// Erasure-coded PUT and GET paths.
//
// The write path reads the full request body into memory, hands it to
// the encoder named by the tenant's placement policy, and writes each
// shard as a separate piece on the chosen backend. The manifest
// records shard position (StripeIndex, ShardIndex, ShardKind) so the
// read path can reconstruct the plaintext even when up to ParityShards
// of the shards per stripe are missing.
//
// Streaming the encode/decode is possible in principle — the
// klauspost/reedsolomon codec supports it — but requires tuning the
// stripe size vs. the HTTP buffer size and coordinating provider
// back-pressure. Phase 3 buffers the whole object; streaming is a
// Phase 4 workstream covered in docs/PROPOSAL.md §6.

package s3compat

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/zeebo/blake3"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/pieceintegrity"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// maxECObjectSize caps the size of an object the EC PUT path is
// willing to buffer into memory before encoding. The path reads the
// entire request body before handing it to the reed-solomon
// encoder; until streaming EC lands (docs/PROPOSAL.md §6) operators
// must keep individual objects below this ceiling or route them
// through a non-EC backend.
//
// Matches MaxInMemoryObjectBytes (handler.go) so operators have a
// single ceiling to reason about across the gateway-encrypted GET,
// multipart pre-fetch, cache-warming, and EC PUT paths.
const maxECObjectSize int64 = MaxInMemoryObjectBytes

// maxMultipartInMemoryBytes is the hard ceiling on the total
// reassembled object size for multipart GETs and the matching
// HEAD pre-flight rejection. Multipart parts are pre-fetched and
// concatenated in memory before being written to the response,
// so any single GET must fit; the pathological request cannot
// OOM the gateway. Both getMultipart and headMultipart reference
// this constant so HEAD/GET agree on the rejection threshold.
// Streaming multipart GETs are a Phase 4 workstream; until that
// lands operators should route very large objects through the EC
// path or a direct-to-backend presigned URL.
const maxMultipartInMemoryBytes int64 = 256 * 1024 * 1024

// ecAuditAttribution computes the (backend, pieceID, country)
// triple used for audit emission on erasure-coded reads. EC
// shards are scattered across one or more backends and the
// gateway has no per-object identity to attribute to; the
// write-side records MigrationState.PrimaryBackend as the
// canonical anchor and Pieces[0].PieceID as the first shard's
// content hash. Both getErasureCoded and headErasureCoded route
// through this helper so the EC PUT/GET/HEAD audit trail is
// uniformly attributed by (PrimaryBackend, Pieces[0].PieceID).
// Country is "" if PrimaryBackend isn't currently in the
// provider map (e.g. mid-migration before the new primary is
// wired); callers that have a resolved fallback country may
// substitute it themselves.
func (h *Handler) ecAuditAttribution(manifest *metadata.ObjectManifest) (backend, pieceID, country string) {
	backend = manifest.MigrationState.PrimaryBackend
	if len(manifest.Pieces) > 0 {
		pieceID = manifest.Pieces[0].PieceID
	}
	if prov, ok := h.cfg.Providers[backend]; ok {
		country = prov.PlacementLabels().Country
	}
	return backend, pieceID, country
}

// multipartAuditAttribution computes the (backend, pieceID,
// country) triple used for audit emission on multipart reads.
// CreateMultipartUpload pins every part of an upload to a single
// backend, so Pieces[0].Backend is the canonical attribution and
// matches the PUT audit emitted from CompleteMultipartUpload.
// Both getMultipart and headMultipart route through this helper
// so HEAD attribution can't silently drift if resolve()'s piece
// selection ever changes.
func (h *Handler) multipartAuditAttribution(manifest *metadata.ObjectManifest) (backend, pieceID, country string) {
	if len(manifest.Pieces) == 0 {
		return "", "", ""
	}
	pieceID = manifest.Pieces[0].PieceID
	backend = manifest.Pieces[0].Backend
	if prov, ok := h.cfg.Providers[backend]; ok {
		country = prov.PlacementLabels().Country
	}
	return backend, pieceID, country
}

// putErasureCoded is called by Put when the resolved placement policy
// names an ErasureProfile. It encodes the body into k + m shards per
// stripe and writes each shard as its own piece.
func (h *Handler) putErasureCoded(
	w http.ResponseWriter,
	r *http.Request,
	tenantID, bucket, key, backendName string,
	provider providers.StorageProvider,
	policy metadata.PlacementPolicy,
) {
	if h.cfg.ErasureCoding == nil {
		writeError(w, http.StatusInternalServerError, "InvalidPlacement",
			"placement policy specifies erasure profile "+policy.ErasureProfile+" but no registry is configured",
			r.URL.Path)
		return
	}
	encoder, err := h.cfg.ErasureCoding.Lookup(policy.ErasureProfile)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidPlacement", err.Error(), r.URL.Path)
		return
	}

	// The EC PUT path buffers the entire request body in memory
	// before encoding into k+m shards; the klauspost/reedsolomon
	// codec supports streaming but the wiring is a Phase 4
	// workstream (docs/PROPOSAL.md §6). Until streaming EC lands,
	// reject requests whose Content-Length advertises a body above
	// the in-memory ceiling so a single client cannot OOM the
	// gateway. The check matches MaxInMemoryObjectBytes so
	// operators have a single knob for both the EC and the
	// gateway-encrypted GET paths.
	if r.ContentLength > maxECObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "ECObjectTooLarge",
			fmt.Sprintf("erasure-coded object of %d bytes exceeds in-memory encode ceiling of %d bytes; streaming EC is not yet implemented",
				r.ContentLength, maxECObjectSize),
			r.URL.Path)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxECObjectSize+1))
	if err != nil {
		// dispatch wraps r.Body in MaxBytesReader, so a client
		// that overflows MaxRequestBytes surfaces here as
		// *http.MaxBytesError. Route through writeBodyReadError
		// so EC clients see the same 413 EntityTooLarge as
		// every other body-reading path; falls back to 400
		// InvalidArgument for any other read failure.
		writeBodyReadError(w, r, err)
		return
	}
	if int64(len(body)) > maxECObjectSize {
		writeError(w, http.StatusRequestEntityTooLarge, "ECObjectTooLarge",
			fmt.Sprintf("erasure-coded object exceeds in-memory encode ceiling of %d bytes; streaming EC is not yet implemented",
				maxECObjectSize),
			r.URL.Path)
		return
	}
	plaintextSize := int64(len(body))

	// Encrypt BEFORE erasure-coding so every shard is ciphertext.
	// A partial shard recovery therefore leaks nothing about the
	// plaintext layout. For client_side mode the body is already
	// ciphertext (the tenant encrypted before PUT); the gateway
	// erasure-codes the opaque bytes verbatim.
	encMode := policy.EncryptionMode
	// Generate the version BEFORE encryption so the AAD v1 binding
	// can fix it into every chunk's tag; the manifest records the
	// same VersionID so the GET path rebuilds the identical AAD.
	versionID := newPieceID(tenantID, bucket, key, h.cfg.Now())
	encID := aadIdentity{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     versionID,
	}
	encCfg, prepared, prepareOK := h.prepareErasureCodedEncryption(w, r, encMode, body, encID)
	if !prepareOK {
		return
	}
	body = prepared

	shards, err := encoder.Encode(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ErasureEncodeFailed", err.Error(), r.URL.Path)
		return
	}

	pieces := make([]metadata.Piece, 0, len(shards))
	written := make([]string, 0, len(shards))
	for _, shard := range shards {
		shardID := fmt.Sprintf("%s-s%04d-p%03d", versionID, shard.StripeIndex, shard.ShardIndex)
		res, putErr := provider.PutPiece(r.Context(), shardID, bytes.NewReader(shard.Bytes), providers.PutOptions{
			ContentLength: int64(len(shard.Bytes)),
			ContentType:   r.Header.Get("Content-Type"),
		})
		if putErr != nil {
			rollbackEC(r, h.cfg.Providers, provider, backendName, written)
			writeError(w, http.StatusBadGateway, "BackendPutFailed", putErr.Error(), r.URL.Path)
			return
		}
		written = append(written, res.PieceID)
		kind := metadata.ShardKindData
		if shard.Kind == erasure_coding.ShardKindParity {
			kind = metadata.ShardKindParity
		}
		// Hash is the BLAKE3 of the shard bytes — what the GET
		// integrity check re-computes from the shard payload.
		// ProviderETag holds the backend's opaque ETag for the
		// upload (some backends return a multipart-style
		// concatenated MD5 that has no relationship to the
		// bytes; we cannot use it for verification).
		shardHash := blake3.Sum256(shard.Bytes)
		pieces = append(pieces, metadata.Piece{
			PieceID:      res.PieceID,
			Hash:         "blake3:" + hex.EncodeToString(shardHash[:]),
			ProviderETag: res.ETag,
			Backend:      backendName,
			Locator:      res.Locator,
			State:        "active",
			SizeBytes:    int64(len(shard.Bytes)),
			StripeIndex:  shard.StripeIndex,
			ShardIndex:   shard.ShardIndex,
			ShardKind:    kind,
		})
	}

	manifest := &metadata.ObjectManifest{
		TenantID:        tenantID,
		Bucket:          bucket,
		ObjectKey:       key,
		ObjectKeyHash:   hashObjectKey(key),
		VersionID:       versionID,
		ObjectSize:      plaintextSize,
		ChunkSize:       int64(encoder.ShardSize()),
		Encryption:      encCfg,
		PlacementPolicy: policy,
		Pieces:          pieces,
		MigrationState: metadata.MigrationState{
			Generation:     1,
			PrimaryBackend: backendName,
		},
		CreatedAt: h.cfg.Now(),
	}
	if err := h.applyDefaultObjectLockRetention(r.Context(), tenantID, bucket, manifest); err != nil {
		rollbackEC(r, h.cfg.Providers, provider, backendName, written)
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return
	}
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: manifest.ObjectKeyHash,
		VersionID:     manifest.VersionID,
	}
	if err := h.cfg.Manifests.Put(r.Context(), mkey, manifest); err != nil {
		rollbackEC(r, h.cfg.Providers, provider, backendName, written)
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}

	h.emit(tenantID, bucket, billing.PutRequests, 1)
	var totalShardBytes uint64
	for _, p := range pieces {
		totalShardBytes += uint64(p.SizeBytes)
	}
	if totalShardBytes > 0 {
		h.emit(tenantID, bucket, billing.StorageBytesSeconds, totalShardBytes)
	}
	h.audit(r, "PUT", tenantID, bucket, key, manifest.Pieces[0].PieceID, backendName, provider.PlacementLabels().Country)
	h.notify(r, eventObjectCreatedPut, tenantID, bucket, key, "", manifest.VersionID, manifest.ObjectSize)

	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
}

// getErasureCoded reconstructs the plaintext from the shards named in
// manifest.Pieces and serves it whole (200) or, when the request
// carries a Range header, as a byte-range slice (206 + Content-Range).
// The full object is reconstructed in memory regardless, so slicing
// the already-materialised plaintext costs nothing extra. Streaming
// range reads that fetch only the stripes overlapping the range remain
// a Phase 4 optimisation (docs/PROPOSAL.md §6); this buffered path
// gives correct S3-compatible Range semantics in the meantime.
func (h *Handler) getErasureCoded(
	w http.ResponseWriter,
	r *http.Request,
	manifest *metadata.ObjectManifest,
	tenantID, bucket string,
) {
	if h.cfg.ErasureCoding == nil {
		writeError(w, http.StatusInternalServerError, "ErasureCodingNotConfigured",
			"object is erasure-coded but no registry is configured", r.URL.Path)
		return
	}
	profile := manifest.PlacementPolicy.ErasureProfile
	if profile == "" {
		// The manifest was produced by EC (shard metadata populated)
		// but dropped the profile name. Attempt inference by looking
		// up any profile whose (k, m) matches the piece layout.
		writeError(w, http.StatusInternalServerError, "ErasureProfileMissing",
			"erasure-coded manifest is missing ErasureProfile", r.URL.Path)
		return
	}
	encoder, err := h.cfg.ErasureCoding.Lookup(profile)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ErasureProfileNotRegistered", err.Error(), r.URL.Path)
		return
	}
	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}

	total := encoder.Profile().TotalShards()
	numStripes := 0
	for _, p := range manifest.Pieces {
		if p.StripeIndex+1 > numStripes {
			numStripes = p.StripeIndex + 1
		}
	}
	if numStripes == 0 {
		writeError(w, http.StatusInternalServerError, "EmptyManifest",
			"erasure-coded manifest has no stripes", r.URL.Path)
		return
	}

	// Stable ordering helps the fetcher report meaningful errors.
	pieces := make([]metadata.Piece, len(manifest.Pieces))
	copy(pieces, manifest.Pieces)
	sort.Slice(pieces, func(i, j int) bool {
		if pieces[i].StripeIndex != pieces[j].StripeIndex {
			return pieces[i].StripeIndex < pieces[j].StripeIndex
		}
		return pieces[i].ShardIndex < pieces[j].ShardIndex
	})

	shards := make([]erasure_coding.Shard, 0, numStripes*total)
	tolerance := encoder.Profile().ParityShards
	losses := make([]int, numStripes)
	for _, p := range pieces {
		prov, ok := h.cfg.Providers[p.Backend]
		if !ok {
			losses[p.StripeIndex]++
			shards = append(shards, erasure_coding.Shard{
				StripeIndex: p.StripeIndex,
				ShardIndex:  p.ShardIndex,
				Kind:        shardKindFromManifest(p.ShardKind),
			})
			if losses[p.StripeIndex] > tolerance {
				writeError(w, http.StatusBadGateway, "DataLoss",
					fmt.Sprintf("stripe %d exceeded parity tolerance: backend %q not registered", p.StripeIndex, p.Backend),
					r.URL.Path)
				return
			}
			continue
		}
		body, getErr := prov.GetPiece(r.Context(), p.PieceID, nil)
		if getErr != nil {
			losses[p.StripeIndex]++
			shards = append(shards, erasure_coding.Shard{
				StripeIndex: p.StripeIndex,
				ShardIndex:  p.ShardIndex,
				Kind:        shardKindFromManifest(p.ShardKind),
			})
			if losses[p.StripeIndex] > tolerance {
				writeError(w, http.StatusBadGateway, "DataLoss",
					fmt.Sprintf("stripe %d exceeded parity tolerance: %v", p.StripeIndex, getErr),
					r.URL.Path)
				return
			}
			continue
		}
		buf, rerr := io.ReadAll(body)
		_ = body.Close()
		if rerr != nil {
			writeError(w, http.StatusBadGateway, "BackendGetFailed", rerr.Error(), r.URL.Path)
			return
		}

		// Verify the shard bytes match the manifest's per-shard
		// BLAKE3 hash before handing them to the EC decoder. A
		// silent corruption that preserves the shard length
		// would otherwise feed bad bytes into Reed-Solomon, and
		// the decoder cannot tell tampered data from good data
		// when it has enough shards to "reconstruct" without
		// rebuilding from parity. Treat a content mismatch like a
		// missing shard: count it as a loss, emit the per-backend
		// metric so operators see the bit-rot signal even when
		// parity recovers, and let the parity-tolerance gate
		// below decide whether the stripe is still recoverable.
		// An unrecognised hash format (legacy manifest with an
		// opaque ETag in Hash) is reported on the dedicated
		// observability channel; the shard's bytes are still fed
		// to the decoder because we cannot prove they're wrong.
		if verr := pieceintegrity.Verify(buf, p); verr != nil {
			if errors.Is(verr, pieceintegrity.ErrIntegrityClaimUnrecognized) {
				h.recordIntegrityUnrecognized(p, verr)
			} else {
				h.recordIntegrityFailure(p, verr)
				losses[p.StripeIndex]++
				shards = append(shards, erasure_coding.Shard{
					StripeIndex: p.StripeIndex,
					ShardIndex:  p.ShardIndex,
					Kind:        shardKindFromManifest(p.ShardKind),
				})
				if losses[p.StripeIndex] > tolerance {
					writeError(w, http.StatusBadGateway, "IntegrityCheckFailed",
						fmt.Sprintf("stripe %d exceeded parity tolerance after shard integrity failure: %v", p.StripeIndex, verr),
						r.URL.Path)
					return
				}
				continue
			}
		}

		shards = append(shards, erasure_coding.Shard{
			StripeIndex: p.StripeIndex,
			ShardIndex:  p.ShardIndex,
			Kind:        shardKindFromManifest(p.ShardKind),
			Bytes:       buf,
		})
	}

	decoded, err := encoder.Decode(shards)
	if err != nil {
		writeError(w, http.StatusBadGateway, "ErasureDecodeFailed", err.Error(), r.URL.Path)
		return
	}

	// For managed / public_distribution objects the encoder's
	// output is the ciphertext the gateway produced in
	// prepareErasureCodedEncryption; we unseal it before handing
	// it back. client_side objects stay opaque.
	plaintext := decoded
	if IsGatewayEncrypted(manifest.Encryption.Mode) {
		decrypted, derr := h.decryptFromStorage(decoded, manifest.Encryption, aadIdentityOf(manifest))
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
			return
		}
		plaintext = decrypted
	}

	out := plaintext
	status := http.StatusOK
	if byteRange != nil {
		end := byteRange.End
		if end < 0 || end >= int64(len(plaintext)) {
			end = int64(len(plaintext)) - 1
		}
		if byteRange.Start < 0 || byteRange.Start > end+1 {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "range out of bounds", r.URL.Path)
			return
		}
		out = plaintext[byteRange.Start : end+1]
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		status = http.StatusPartialContent
	}

	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(out)))
	w.WriteHeader(status)
	n, _ := w.Write(out)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if n > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(n))
		h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
	}

	// Audit the EC GET so the compliance trail is symmetric with
	// the single-piece GET path. ecAuditAttribution centralises
	// the (PrimaryBackend, Pieces[0].PieceID) anchor so HEAD and
	// GET can't drift on the audit row's backend/piece columns.
	auditBackend, auditPieceID, auditCountry := h.ecAuditAttribution(manifest)
	h.audit(r, "GET", tenantID, bucket, manifest.ObjectKey, auditPieceID, auditBackend, auditCountry)
}

// isErasureCodedManifest returns true when the manifest's pieces
// carry shard metadata (ShardKind is set on at least one piece).
func isErasureCodedManifest(m *metadata.ObjectManifest) bool {
	for _, p := range m.Pieces {
		if p.ShardKind != "" {
			return true
		}
	}
	return false
}

// isMultipartManifest returns true when the manifest lists more than
// one piece and each piece carries a non-zero PartNumber. The GET
// path concatenates pieces by PartNumber.
func isMultipartManifest(m *metadata.ObjectManifest) bool {
	if len(m.Pieces) < 2 {
		return false
	}
	for _, p := range m.Pieces {
		if p.PartNumber == 0 {
			return false
		}
	}
	return true
}

func shardKindFromManifest(s string) erasure_coding.ShardKind {
	if s == metadata.ShardKindParity {
		return erasure_coding.ShardKindParity
	}
	return erasure_coding.ShardKindData
}

// getMultipart serves a multipart-assembled object by concatenating
// each piece in ascending PartNumber order. It serves the whole
// object (200) or, when the request carries a Range header, a
// byte-range slice (206 + Content-Range) of the assembled plaintext.
//
// All piece backends are verified up front, then every piece body is
// fetched and buffered in memory before the HTTP status line or
// Content-Length header is committed. This mirrors getErasureCoded:
// a GetPiece failure surfaces as a clean 502 instead of a
// silently-truncated response body. Because the whole object is
// already buffered (bounded by maxMultipartInMemoryBytes), serving a
// Range is a slice of that buffer at no extra cost. Streaming
// multipart GETs that fetch only the parts overlapping the range
// remain a Phase 4 workstream tracked in docs/PROPOSAL.md §6.
func (h *Handler) getMultipart(
	w http.ResponseWriter,
	r *http.Request,
	manifest *metadata.ObjectManifest,
	tenantID, bucket string,
) {
	var byteRange *providers.ByteRange
	if hdr := r.Header.Get("Range"); hdr != "" {
		rng, perr := parseHTTPRange(hdr, manifest.ObjectSize)
		if perr != nil {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", perr.Error(), r.URL.Path)
			return
		}
		byteRange = rng
	}

	if manifest.ObjectSize > maxMultipartInMemoryBytes {
		writeError(w, http.StatusInsufficientStorage, "MultipartTooLarge",
			fmt.Sprintf("multipart object of %d bytes exceeds in-memory pre-fetch ceiling of %d bytes",
				manifest.ObjectSize, maxMultipartInMemoryBytes),
			r.URL.Path)
		return
	}

	pieces := make([]metadata.Piece, len(manifest.Pieces))
	copy(pieces, manifest.Pieces)
	sort.Slice(pieces, func(i, j int) bool {
		return pieces[i].PartNumber < pieces[j].PartNumber
	})

	provs := make([]providers.StorageProvider, len(pieces))
	for i, p := range pieces {
		prov, ok := h.cfg.Providers[p.Backend]
		if !ok {
			writeError(w, http.StatusBadGateway, "BackendNotRegistered",
				fmt.Sprintf("part %d references unregistered backend %q", p.PartNumber, p.Backend),
				r.URL.Path)
			return
		}
		provs[i] = prov
	}

	// Pre-fetch every piece body into memory so a backend failure
	// mid-assembly fails cleanly as a 502. Writing the status line
	// before we hold the full object would force us to truncate on
	// a late error; the EC path has the same constraint and
	// resolves it the same way (see getErasureCoded).
	bodies := make([][]byte, len(pieces))
	for i, p := range pieces {
		body, err := provs[i].GetPiece(r.Context(), p.PieceID, nil)
		if err != nil {
			writeError(w, http.StatusBadGateway, "BackendGetFailed",
				fmt.Sprintf("part %d piece %q: %v", p.PartNumber, p.PieceID, err),
				r.URL.Path)
			return
		}
		buf, rerr := io.ReadAll(body)
		_ = body.Close()
		if rerr != nil {
			writeError(w, http.StatusBadGateway, "BackendGetFailed",
				fmt.Sprintf("part %d piece %q: read: %v", p.PartNumber, p.PieceID, rerr),
				r.URL.Path)
			return
		}

		// Verify the per-part BLAKE3 hash before decryption /
		// concatenation. UploadPart hashes the ciphertext as it
		// streams to the backend (PR-2), so a mismatch here
		// means the backend either lost or tampered with the
		// part. Fail closed on a content mismatch: we have
		// already read the part into memory but have not
		// committed the response status line, so a 502 cleanly
		// aborts the GET before any bytes reach the client. A
		// legacy manifest with an unrecognised hash format gets
		// the observability counter but still serves — there is
		// no proof the bytes are wrong.
		if verr := pieceintegrity.Verify(buf, p); verr != nil {
			if errors.Is(verr, pieceintegrity.ErrIntegrityClaimUnrecognized) {
				h.recordIntegrityUnrecognized(p, verr)
			} else {
				h.recordIntegrityFailure(p, verr)
				writeError(w, http.StatusBadGateway, "IntegrityCheckFailed",
					fmt.Sprintf("part %d piece %q: %v", p.PartNumber, p.PieceID, verr),
					r.URL.Path)
				return
			}
		}
		bodies[i] = buf
	}

	var total int64
	for _, b := range bodies {
		total += int64(len(b))
	}

	// For managed / public_distribution multipart uploads each
	// piece is an independently-sealed ciphertext stream under the
	// session-level DEK. The SDK's framing treats any shorter-
	// than-chunk-size frame as terminal, so we cannot just
	// concatenate the ciphertexts and decrypt once — we decrypt
	// each part in isolation and concatenate the resulting
	// plaintexts. All parts of a single upload share one wrapped
	// DEK, so we unwrap once up front and reuse the plaintext key
	// across every part via decryptWithDEK; this mirrors the
	// write path, where UploadPart calls encryptWithDEK with the
	// session DEK generated at CreateMultipartUpload time.
	// manifest.ObjectSize records the plaintext aggregate so the
	// integrity check below still fires.
	if IsGatewayEncrypted(manifest.Encryption.Mode) {
		if h.cfg.Encryption == nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
				"object is encrypted but no gateway encryption is configured", r.URL.Path)
			return
		}
		dek, uerr := h.cfg.Encryption.Wrapper.UnwrapDEK(encryption.DataEncryptionKey{
			KeyID:         manifest.Encryption.KeyID,
			Algorithm:     manifest.Encryption.Algorithm,
			WrappedKey:    manifest.Encryption.WrappedDEK,
			WrapAlgorithm: manifest.Encryption.WrapAlgorithm,
		}, h.cfg.Encryption.CMK)
		if uerr != nil {
			writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", uerr.Error(), r.URL.Path)
			return
		}
		plaintexts := make([][]byte, len(bodies))
		var newTotal int64
		for i, b := range bodies {
			pt, derr := h.decryptWithDEK(b, dek, manifest.Encryption, aadIdentityOf(manifest))
			if derr != nil {
				writeError(w, http.StatusInternalServerError, "DecryptionFailed", derr.Error(), r.URL.Path)
				return
			}
			plaintexts[i] = pt
			newTotal += int64(len(pt))
		}
		bodies = plaintexts
		total = newTotal
	}

	// Integrity guard: the aggregate of the piece bodies we just
	// pulled from the backends must match the manifest's recorded
	// object size. A mismatch points at either manifest corruption
	// or a backend that served truncated / padded pieces — either
	// way, the client should see a 502 instead of a correct-looking
	// 200 with the wrong Content-Length.
	if manifest.ObjectSize != 0 && total != manifest.ObjectSize {
		writeError(w, http.StatusBadGateway, "ManifestIntegrityMismatch",
			fmt.Sprintf("assembled %d bytes but manifest records %d", total, manifest.ObjectSize),
			r.URL.Path)
		return
	}

	w.Header().Set("x-amz-version-id", manifest.VersionID)

	var written int64
	if byteRange != nil {
		assembled := make([]byte, 0, total)
		for _, b := range bodies {
			assembled = append(assembled, b...)
		}
		end := byteRange.End
		if end < 0 || end >= total {
			end = total - 1
		}
		if byteRange.Start < 0 || byteRange.Start > end+1 {
			writeError(w, http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "range out of bounds", r.URL.Path)
			return
		}
		slice := assembled[byteRange.Start : end+1]
		w.Header().Set("Content-Range", formatContentRange(byteRange, manifest.ObjectSize))
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(slice)))
		w.WriteHeader(http.StatusPartialContent)
		n, _ := w.Write(slice)
		written += int64(n)
	} else {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
		w.WriteHeader(http.StatusOK)
		for _, b := range bodies {
			n, _ := w.Write(b)
			written += int64(n)
		}
	}

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if written > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(written))
		h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(written))
	}

	// Audit the multipart GET so the compliance trail is symmetric
	// with the single-piece GET path. multipartAuditAttribution
	// pins to Pieces[0] so HEAD and GET can't drift if resolve()'s
	// piece selection ever changes.
	auditBackend, auditPieceID, auditCountry := h.multipartAuditAttribution(manifest)
	h.audit(r, "GET", tenantID, bucket, manifest.ObjectKey, auditPieceID, auditBackend, auditCountry)
}

// rollbackEC deletes pieces written during a failed EC put so the
// backend isn't left with orphaned shards.
func rollbackEC(
	r *http.Request,
	providers map[string]providers.StorageProvider,
	primary providers.StorageProvider,
	backendName string,
	pieceIDs []string,
) {
	prov := primary
	if p, ok := providers[backendName]; ok {
		prov = p
	}
	for _, id := range pieceIDs {
		_ = prov.DeletePiece(r.Context(), id)
	}
}
