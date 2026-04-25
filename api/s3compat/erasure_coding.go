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
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "read body: "+err.Error(), r.URL.Path)
		return
	}
	plaintextSize := int64(len(body))

	// Encrypt BEFORE erasure-coding so every shard is ciphertext.
	// A partial shard recovery therefore leaks nothing about the
	// plaintext layout. For client_side mode the body is already
	// ciphertext (the tenant encrypted before PUT); the gateway
	// erasure-codes the opaque bytes verbatim.
	encMode := policy.EncryptionMode
	encCfg, prepared, prepareOK := h.prepareErasureCodedEncryption(w, r, encMode, body)
	if !prepareOK {
		return
	}
	body = prepared

	shards, err := encoder.Encode(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ErasureEncodeFailed", err.Error(), r.URL.Path)
		return
	}

	versionID := newPieceID(tenantID, bucket, key, h.cfg.Now())
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
		pieces = append(pieces, metadata.Piece{
			PieceID:     res.PieceID,
			Hash:        res.ETag,
			Backend:     backendName,
			Locator:     res.Locator,
			State:       "active",
			SizeBytes:   int64(len(shard.Bytes)),
			StripeIndex: shard.StripeIndex,
			ShardIndex:  shard.ShardIndex,
			ShardKind:   kind,
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

	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.WriteHeader(http.StatusOK)
}

// getErasureCoded reconstructs the plaintext from the shards named in
// manifest.Pieces. Range reads are not supported on EC objects in
// Phase 3; the handler returns the full object and leaves range-read
// support to Phase 4 streaming work.
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
	if r.Header.Get("Range") != "" {
		writeError(w, http.StatusNotImplemented, "NotImplemented",
			"range reads on erasure-coded objects are not yet supported", r.URL.Path)
		return
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
		decrypted, derr := h.decryptFromStorage(decoded, manifest.Encryption)
		if derr != nil {
			writeError(w, http.StatusInternalServerError, "DEKUnwrapFailed", derr.Error(), r.URL.Path)
			return
		}
		plaintext = decrypted
	}

	w.Header().Set("x-amz-version-id", manifest.VersionID)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(plaintext)))
	w.WriteHeader(http.StatusOK)
	n, _ := w.Write(plaintext)

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if n > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(n))
		h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(n))
	}
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
// each piece in ascending PartNumber order. Range reads are not yet
// supported on multipart manifests; S3 SDKs do not rely on ranged
// reads for multipart downloads, so this is a Phase 4 workstream.
//
// All piece backends are verified up front, then every piece body is
// fetched and buffered in memory before the HTTP status line or
// Content-Length header is committed. This mirrors getErasureCoded:
// a GetPiece failure surfaces as a clean 502 instead of a
// silently-truncated response body. The whole-object buffering
// trade-off matches Phase 3's EC path; streaming multipart GETs
// are a Phase 4 workstream tracked in docs/PROPOSAL.md §6.
func (h *Handler) getMultipart(
	w http.ResponseWriter,
	r *http.Request,
	manifest *metadata.ObjectManifest,
	tenantID, bucket string,
) {
	if r.Header.Get("Range") != "" {
		writeError(w, http.StatusNotImplemented, "NotImplemented",
			"range reads on multipart objects are not yet supported", r.URL.Path)
		return
	}

	// maxMultipartInMemoryBytes is the hard ceiling on the total
	// manifest size the pre-fetch path will buffer. Multipart GETs
	// above this ceiling are rejected up front with 507 so a
	// pathological request cannot OOM the gateway. Streaming
	// multipart GETs are a Phase 4 workstream; until that lands
	// operators should route very large objects through the EC
	// path or a direct-to-backend presigned URL.
	const maxMultipartInMemoryBytes int64 = 256 * 1024 * 1024

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
			pt, derr := h.decryptWithDEK(b, dek)
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
	w.Header().Set("Content-Length", fmt.Sprintf("%d", total))
	w.WriteHeader(http.StatusOK)

	var written int64
	for _, b := range bodies {
		n, _ := w.Write(b)
		written += int64(n)
	}

	h.emit(tenantID, bucket, billing.GetRequests, 1)
	if written > 0 {
		h.emit(tenantID, bucket, billing.EgressBytes, uint64(written))
		h.emit(tenantID, bucket, billing.OriginEgressBytes, uint64(written))
	}
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
