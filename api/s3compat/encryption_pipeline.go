// Encryption pipeline helpers shared by the single-piece, erasure-
// coded, and multipart write/read paths.
//
// These helpers consolidate the three concerns that every encryption
// branch needs: (1) resolving the per-object DEK and its wrap, (2)
// encrypting plaintext to ciphertext for storage, and (3) decrypting
// ciphertext back to plaintext at read time. Keeping them in one
// place avoids drift between the data-path branches and means the
// Phase 3 KMS migration touches a single call site per path.

package s3compat

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// IsGatewayEncrypted reports whether the given encryption mode needs
// the gateway to seal / open object bytes on behalf of the tenant.
// "client_side" is intentionally excluded: in Strict ZK mode the
// client handles all cryptography and the gateway must treat
// ciphertext as opaque bytes.
func IsGatewayEncrypted(mode string) bool {
	return mode == string(encryption.ManagedEncrypted) ||
		mode == string(encryption.PublicDistribution)
}

// gatewayEncryptOptions returns the canonical client_sdk.Options
// used by every non-convergent gateway encrypt path: the
// single-piece PUT (buffered + streaming), the erasure-coded PUT,
// the multipart UploadPart path, and the EncryptedSize prediction
// used to advertise the backend's ContentLength on the streaming
// path.
//
// Centralising this value closes the latent maintenance hazard
// noted in PR #74 review (3299180226): the streaming PUT path
// calls EncryptedSize(plaintextSize, Options{}) and then
// streamEncryptForStorage runs EncryptObject(stream, dek,
// Options{}) — two independent literals that MUST stay in sync.
// If a future change makes ChunkSize configurable (or adds any
// other SDK option that affects ciphertext length) and only one
// callsite is updated, the gateway would advertise a Content-
// Length that no longer matches the actual ciphertext, producing
// either truncated uploads (predicted size < emitted size) or
// hanging connections (predicted size > emitted size, backend
// waits for trailing bytes).
//
// Returning Options by value keeps the call surface unchanged
// while making the coupling explicit at the type level.
// Convergent-encryption paths (dedup, multipart EncryptObject
// callsite that sets ConvergentNonce: true) intentionally do not
// use this helper because they require the convergent-nonce
// derivation — but those paths also do not call EncryptedSize, so
// the size-prediction coupling does not apply.
func gatewayEncryptOptions() client_sdk.Options {
	return client_sdk.Options{}
}

// encryptForStorage seals plaintext with a freshly-generated DEK and
// returns (ciphertext, wrapped DEK, error). The wrapped DEK is what
// the caller stores on the manifest. The plaintext DEK is owned
// entirely by this function and is scrubbed before return as
// defence-in-depth against heap-dump / paged-out-memory exposure of
// raw key material; multipart-style callers that need a stable DEK
// across a sequence of encrypt calls must use encryptWithDEK with a
// caller-managed DEK instead.
//
// PUT-path callers should prefer streamEncryptForStorage when the
// client supplied a Content-Length: the streaming reader pipes the
// SDK's chunk-by-chunk output straight to the backend instead of
// materialising the full ciphertext in memory. encryptForStorage
// remains in use for the erasure-coded path (which always buffers
// because the EC encoder shards a fully-formed byte slice) and the
// chunked / unknown-length single-piece fallback.
func (h *Handler) encryptForStorage(plaintext []byte) ([]byte, client_sdk.WrappedDEK, error) {
	if h.cfg.Encryption == nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: gateway encryption is not configured")
	}
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: generate dek: %w", err)
	}
	// DEK scrubbing via defer: zero the raw DEK on every return
	// path (success and error). Pre-fix the scrub only ran on the
	// success path, so an intermediate failure (EncryptObject /
	// io.ReadAll / WrapDEK) would leave the raw key bytes in the
	// goroutine's heap until GC. Move the scrub to a defer so the
	// defence-in-depth window is symmetric across all error
	// paths, closing the gap noted in PR #74 review (3299180089).
	// The SDK's EncryptObject always copies the DEK into the
	// encryptReader before returning (see
	// encryption/client_sdk/sdk.go: EncryptObject), so clearing
	// the caller's backing array is safe in both random-nonce and
	// convergent-nonce modes without corrupting the in-flight
	// stream.
	defer clear(dek)
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, gatewayEncryptOptions())
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: read ciphertext: %w", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: wrap dek: %w", err)
	}
	return ciphertext, wrapped, nil
}

// streamEncryptForStorage is the streaming mirror of
// encryptForStorage: it generates a fresh DEK, wraps it, and
// returns the SDK's encrypt reader so the caller can io.Copy
// ciphertext directly to the backend without ever buffering the
// full plaintext or ciphertext in memory.
//
// The SDK's EncryptObject already streams chunk-by-chunk
// (encryption/client_sdk/sdk.go: encryptReader.nextFrame), so the
// gateway gains nothing from the buffered encryptForStorage's
// io.ReadAll except a 2x memory spike per concurrent PUT. The
// single-piece PUT path uses this helper when the client supplied
// a Content-Length so the backend can be handed a known ciphertext
// size; the buffered helper remains for the dedup path (which
// content-addresses the ciphertext) and the EC path (which
// requires the full plaintext in memory to shard).
//
// Encrypt errors surface on the first Read of the returned reader,
// not on this function returning, so the caller MUST drain the
// reader to EOF (or propagate any returned error) to learn whether
// the stream was valid.
func (h *Handler) streamEncryptForStorage(plaintext io.Reader) (io.Reader, client_sdk.WrappedDEK, error) {
	if h.cfg.Encryption == nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: gateway encryption is not configured")
	}
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: generate dek: %w", err)
	}
	// DEK scrubbing via defer: zero the raw DEK on every return
	// path (success and error). Pre-fix the scrub only ran on the
	// success path, so an intermediate EncryptObject or WrapDEK
	// failure would leave the raw key bytes in the goroutine's
	// heap until GC. Move the scrub to a defer so the
	// defence-in-depth window is symmetric across all error
	// paths, closing the gap noted in PR #74 review (3299180089).
	// The SDK's EncryptObject always copies the DEK into the
	// encryptReader before returning, so clearing the caller's
	// backing array does not corrupt the in-flight stream — even
	// though the encReader is the value we return to the caller
	// for streaming, the AEAD inside it is keyed off an internal
	// copy.
	defer clear(dek)
	encReader, err := client_sdk.EncryptObject(plaintext, dek, gatewayEncryptOptions())
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: wrap dek: %w", err)
	}
	return encReader, wrapped, nil
}

// encryptWithDEK seals plaintext with an already-generated DEK. It
// is used by the multipart path so every part of a single upload
// shares the same key: the DEK is generated at CreateMultipartUpload
// time, wrapped once, and then handed to every UploadPart call.
func (h *Handler) encryptWithDEK(plaintext []byte, dek client_sdk.DataEncryptionKey) ([]byte, error) {
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, gatewayEncryptOptions())
	if err != nil {
		return nil, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		return nil, fmt.Errorf("s3compat: read ciphertext: %w", err)
	}
	return ciphertext, nil
}

// decryptFromStorage unwraps the DEK recorded on the manifest and
// returns the plaintext for ciphertext. It is the mirror of
// encryptForStorage and encryptWithDEK.
//
// Read-path callers should prefer streamDecryptFromStorage so a
// multi-GiB GET does not balloon the gateway's heap by 2x the
// object size. decryptFromStorage remains in use only on paths
// that genuinely need the full plaintext buffer in memory — the
// gateway-encrypted Range GET path (slices an arbitrary byte
// range out of the materialised plaintext) and the multipart /
// EC GET paths (assemble per-part plaintext blobs before stitching
// them together).
func (h *Handler) decryptFromStorage(ciphertext []byte, enc metadata.EncryptionConfig) ([]byte, error) {
	if h.cfg.Encryption == nil {
		return nil, fmt.Errorf("s3compat: gateway encryption is not configured")
	}
	wrapped := encryption.DataEncryptionKey{
		KeyID:         enc.KeyID,
		Algorithm:     enc.Algorithm,
		WrappedKey:    enc.WrappedDEK,
		WrapAlgorithm: enc.WrapAlgorithm,
	}
	dek, err := h.cfg.Encryption.Wrapper.UnwrapDEK(wrapped, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, fmt.Errorf("s3compat: unwrap dek: %w", err)
	}
	plaintext, derr := h.decryptWithDEK(ciphertext, dek)
	// DEK scrubbing on the read side mirrors the encrypt path:
	// once decryptWithDEK has built its chacha20poly1305 AEAD (which
	// holds its own key schedule) and drained the ciphertext into
	// plaintext, the raw unwrapped DEK is no longer needed. Clearing
	// it bounds heap-dump / paged-out memory exposure of the
	// recovered key bytes. Mirrors the encrypt-side scrub from PR
	// #74 and closes the symmetry gap noted in review.
	clear(dek)
	return plaintext, derr
}

// decryptWithDEK runs the SDK decrypt reader against an already-
// unwrapped DEK. Used by the multipart GET path so parts that
// share a key are decrypted without repeated unwraps.
func (h *Handler) decryptWithDEK(ciphertext []byte, dek client_sdk.DataEncryptionKey) ([]byte, error) {
	decReader, err := client_sdk.DecryptObject(bytes.NewReader(ciphertext), dek, client_sdk.Options{})
	if err != nil {
		return nil, fmt.Errorf("s3compat: decrypt object: %w", err)
	}
	plaintext, err := io.ReadAll(decReader)
	if err != nil {
		return nil, fmt.Errorf("s3compat: read plaintext: %w", err)
	}
	return plaintext, nil
}

// streamDecryptFromStorage is the streaming mirror of
// decryptFromStorage: it unwraps the DEK and returns the SDK's
// decrypt reader so the caller can io.Copy plaintext straight to
// the client without ever buffering the full object in memory.
//
// The SDK's DecryptObject already streams chunk-by-chunk
// (encryption/client_sdk/sdk.go), so the gateway gains nothing from
// the legacy decryptFromStorage's io.ReadAll except a hard ceiling
// at MaxInMemoryObjectBytes and a fat memory spike per concurrent
// request. The non-range GET path uses this helper to lift that
// ceiling; the range-request path keeps decryptFromStorage because
// it needs the full plaintext in memory to slice arbitrary byte
// ranges (chunk-level range seek lands in v0.2.0).
//
// The returned reader takes ownership of ciphertext: callers must
// not read from ciphertext after this returns. Decrypt errors
// surface on the first Read call, not on this returning function,
// so the caller MUST drain to EOF (or close) to learn whether the
// stream was valid.
func (h *Handler) streamDecryptFromStorage(ciphertext io.Reader, enc metadata.EncryptionConfig) (io.Reader, error) {
	if h.cfg.Encryption == nil {
		return nil, fmt.Errorf("s3compat: gateway encryption is not configured")
	}
	wrapped := encryption.DataEncryptionKey{
		KeyID:         enc.KeyID,
		Algorithm:     enc.Algorithm,
		WrappedKey:    enc.WrappedDEK,
		WrapAlgorithm: enc.WrapAlgorithm,
	}
	dek, err := h.cfg.Encryption.Wrapper.UnwrapDEK(wrapped, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, fmt.Errorf("s3compat: unwrap dek: %w", err)
	}
	decReader, err := client_sdk.DecryptObject(ciphertext, dek, client_sdk.Options{})
	if err != nil {
		return nil, fmt.Errorf("s3compat: decrypt object: %w", err)
	}
	// DEK scrubbing on the read side: DecryptObject builds an
	// independent chacha20poly1305 AEAD before returning, so the
	// caller's DEK backing array is safe to clear without
	// corrupting the in-flight decrypt stream. Mirrors the encrypt-
	// side scrub and closes the symmetry gap noted in PR #74
	// review.
	clear(dek)
	return decReader, nil
}

// byteCountingReader wraps an io.Reader and tracks the cumulative
// number of bytes successfully read. It is the streaming PUT path's
// answer to the manifest-ObjectSize question: "how many bytes did
// the client *actually* send?" — distinct from "how many bytes did
// the client *claim* via Content-Length?". The buffered path can
// just call len(plaintext) on the io.ReadAll result; the streaming
// path drains the body through the SDK and never holds the whole
// plaintext, so it needs a side-channel counter.
//
// Read errors do not increment the counter (the contract follows
// io.Reader: n is the number of bytes the caller can rely on).
type byteCountingReader struct {
	R         io.Reader
	bytesRead int64
}

func (c *byteCountingReader) Read(p []byte) (int, error) {
	n, err := c.R.Read(p)
	if n > 0 {
		c.bytesRead += int64(n)
	}
	return n, err
}

// BytesRead is callable at any time but is meaningful only after
// the caller has drained the returned reader (the SDK encrypt
// reader will pull from this counter as it produces ciphertext
// chunks; the count reflects whatever the body actually contained
// when EOF was reached).
func (c *byteCountingReader) BytesRead() int64 { return c.bytesRead }

// prepareSinglePieceEncryption consumes r.Body, applies the
// encryption mode dictated by the tenant's policy, and returns the
// body reader the gateway should hand to the storage backend, its
// content length, a callback that yields the actual plaintext
// bytes consumed (for manifest.ObjectSize), and the
// EncryptionConfig to record on the manifest. A false final return
// indicates the helper already wrote a response and the caller
// should return.
//
// The actual-size callback is the architectural answer to the
// streaming path's manifest-fidelity problem: r.ContentLength is a
// client claim, not ground truth. The buffered path closes over
// len(plaintext) (truth at io.ReadAll time); the streaming path
// closes over the byteCountingReader (truth after the SDK has
// drained the body to EOF). Callers MUST invoke the callback only
// AFTER the returned body reader has been fully drained — reading
// it before drain yields whatever partial count has accumulated.
//
//   - managed / public_distribution: the body is encrypted with a
//     fresh DEK, wrapped with the gateway's CMK, and handed to the
//     backend as ciphertext. Streamed when Content-Length is
//     known; buffered otherwise.
//   - client_side: the body is passed through verbatim after the
//     helper verifies the client asserted the encryption via the
//     X-Amz-Meta-Zk-Encryption header. Missing header → 403
//     EncryptionRequired so tenants with Strict ZK policy cannot
//     accidentally upload plaintext.
//   - "" (legacy): no encryption, no manifest.Encryption.
func (h *Handler) prepareSinglePieceEncryption(
	w http.ResponseWriter,
	r *http.Request,
	encMode string,
) (metadata.EncryptionConfig, io.Reader, int64, func() int64, bool) {
	switch encMode {
	case string(encryption.ManagedEncrypted), string(encryption.PublicDistribution):
		if h.cfg.Encryption == nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
				"tenant policy requires managed encryption but no gateway encryption is configured", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, nil, false
		}
		// Streaming path: when the client supplied a Content-Length
		// the SDK can stream chunk-by-chunk straight to the backend
		// instead of materialising the full plaintext + ciphertext
		// in memory. The pre-streaming code did two io.ReadAll
		// passes (request body, then SDK reader) which doubled the
		// gateway heap per concurrent PUT and capped any single PUT
		// at MaxInMemoryObjectBytes.
		if r.ContentLength >= 0 {
			plaintextSize := r.ContentLength
			// EncryptedSize now returns a typed error so the
			// gateway distinguishes overflow (a hostile
			// Content-Length: 9223372036854775807 that would
			// wrap the int64 ciphertext-size computation),
			// negative input (malformed Content-Length header),
			// and the legitimate (0, nil) for empty objects.
			// Pre-refactor the SDK used a 0 sentinel that
			// overloaded "empty input" with "overflow"; the
			// typed return removes the foot-gun for future
			// callers and forces every callsite to acknowledge
			// the failure mode at the type level. Overflow on
			// the streaming PUT path means we cannot honour the
			// client's declared ContentLength without
			// advertising a wrapped or zero ciphertext length
			// to the backend (either silent truncation or
			// header drop); reject with 400 before the encrypt
			// stream is started.
			// Use the same Options the actual encrypt path will use
			// (gatewayEncryptOptions) so the predicted ciphertext
			// size cannot drift from streamEncryptForStorage's
			// emitted size if SDK options grow new size-affecting
			// fields. See gatewayEncryptOptions docs (3299180226).
			ciphertextSize, err := client_sdk.EncryptedSize(plaintextSize, gatewayEncryptOptions())
			if err != nil {
				switch {
				case errors.Is(err, client_sdk.ErrEncryptedSizeOverflow):
					writeError(w, http.StatusBadRequest, "InvalidContentLength",
						"Content-Length too large for encrypted streaming upload (ciphertext size overflows int64)", r.URL.Path)
				case errors.Is(err, client_sdk.ErrEncryptedSizeNegativePlaintext):
					// Defence-in-depth: r.ContentLength >= 0
					// gates this branch so negative is
					// unreachable here, but ErrEncryptedSize
					// NegativePlaintext is the documented
					// rejection if it ever does arrive.
					writeError(w, http.StatusBadRequest, "InvalidContentLength",
						"Content-Length must not be negative", r.URL.Path)
				default:
					// ErrEncryptedSizeInvalidChunkSize or any
					// future error: treat as 500 because
					// chunkSize() comes from gateway config,
					// not the request.
					writeError(w, http.StatusInternalServerError, "EncryptionConfigInvalid", err.Error(), r.URL.Path)
				}
				return metadata.EncryptionConfig{}, nil, 0, nil, false
			}
			// Wrap the body in a counter so handler.go can read
			// the actual plaintext bytes consumed after PutPiece
			// drains the encrypt stream. Without this the manifest
			// would record the client's CLAIMED Content-Length
			// instead of ground truth — a client that lies and
			// sends fewer bytes than declared would leave a
			// manifest pointing at a backend object whose size
			// does not match the manifest's ObjectSize.
			counter := &byteCountingReader{R: r.Body}
			encReader, wrapped, err := h.streamEncryptForStorage(counter)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
				return metadata.EncryptionConfig{}, nil, 0, nil, false
			}
			cfg := metadata.EncryptionConfig{
				Mode:          encMode,
				Algorithm:     client_sdk.ContentAlgorithm,
				KeyID:         wrapped.KeyID,
				WrappedDEK:    wrapped.WrappedKey,
				WrapAlgorithm: wrapped.WrapAlgorithm,
			}
			return cfg, encReader, ciphertextSize, counter.BytesRead, true
		}
		// Fallback buffered path: chunked or unknown-length
		// uploads (Content-Length: -1) cannot be streamed because
		// the backend wants a known ContentLength. Read the body,
		// encrypt, then explicitly zero the plaintext slice as a
		// defence-in-depth measure — the buffer is held by the
		// goroutine handling the request until GC reclaims it, and
		// without scrubbing a heap dump or paged-out memory could
		// reveal cleartext after the encryption completes.
		plaintext, err := io.ReadAll(r.Body)
		if err != nil {
			writeBodyReadError(w, r, err)
			return metadata.EncryptionConfig{}, nil, 0, nil, false
		}
		ciphertext, wrapped, err := h.encryptForStorage(plaintext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, nil, false
		}
		plaintextSize := int64(len(plaintext))
		// Plaintext scrubbing: zero the buffer now that the SDK
		// has consumed it. Tracking the slice header is enough
		// because Go's escape analysis keeps the underlying array
		// alive only via this reference inside the handler.
		clear(plaintext)
		cfg := metadata.EncryptionConfig{
			Mode:          encMode,
			Algorithm:     client_sdk.ContentAlgorithm,
			KeyID:         wrapped.KeyID,
			WrappedDEK:    wrapped.WrappedKey,
			WrapAlgorithm: wrapped.WrapAlgorithm,
		}
		// Buffered path: plaintextSize captured here IS ground
		// truth (io.ReadAll consumed the body to EOF), so the
		// closure is constant.
		return cfg, bytes.NewReader(ciphertext), int64(len(ciphertext)), func() int64 { return plaintextSize }, true

	case string(encryption.StrictZK):
		algo := r.Header.Get("X-Amz-Meta-Zk-Encryption")
		if algo == "" {
			writeError(w, http.StatusForbidden, "EncryptionRequired",
				"tenant policy requires client_side encryption; set X-Amz-Meta-Zk-Encryption header", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, nil, false
		}
		// Pass the client's ciphertext through unchanged. The
		// gateway does not read it, does not own the DEK, and
		// does not record wrapping material on the manifest.
		// ObjectSize for StrictZK comes from putRes.SizeBytes
		// (the backend-reported ciphertext size), so the
		// plaintext-size closure is irrelevant here — we still
		// return r.ContentLength for handler symmetry but it
		// is not consulted by manifest write.
		cfg := metadata.EncryptionConfig{
			Mode:      encMode,
			Algorithm: algo,
		}
		claimed := r.ContentLength
		return cfg, r.Body, r.ContentLength, func() int64 { return claimed }, true
	}

	// Empty mode = legacy / no encryption. The body is passed
	// through verbatim and manifest.Encryption stays zero-valued.
	legacyClaimed := r.ContentLength
	return metadata.EncryptionConfig{}, r.Body, r.ContentLength, func() int64 { return legacyClaimed }, true
}

// prepareErasureCodedEncryption seals the already-buffered body for
// the tenant's encryption mode before it is handed to the erasure
// encoder. Unlike prepareSinglePieceEncryption the body is already
// in memory here (EC always buffers), so the helper returns the
// ciphertext bytes directly along with the EncryptionConfig to
// record on the manifest.
//
// For client_side the body is the client's ciphertext; the gateway
// erasure-codes it verbatim and stores the tenant-declared
// algorithm on the manifest. A missing X-Amz-Meta-Zk-Encryption
// header returns 403, same as the single-piece path.
func (h *Handler) prepareErasureCodedEncryption(
	w http.ResponseWriter,
	r *http.Request,
	encMode string,
	plaintext []byte,
) (metadata.EncryptionConfig, []byte, bool) {
	switch encMode {
	case string(encryption.ManagedEncrypted), string(encryption.PublicDistribution):
		if h.cfg.Encryption == nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
				"tenant policy requires managed encryption but no gateway encryption is configured", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, false
		}
		ciphertext, wrapped, err := h.encryptForStorage(plaintext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
			return metadata.EncryptionConfig{}, nil, false
		}
		// Plaintext scrubbing: zero the caller's buffer once the
		// SDK has emitted ciphertext. The EC path is the only
		// caller and it still needs `plaintext` for its return
		// metadata in the StrictZK / legacy branches below, but in
		// the managed branch we own the buffer and the caller
		// never reads it again. Defence-in-depth: a heap dump or
		// paged-out memory taken between encrypt and GC could
		// otherwise reveal cleartext.
		clear(plaintext)
		return metadata.EncryptionConfig{
			Mode:          encMode,
			Algorithm:     client_sdk.ContentAlgorithm,
			KeyID:         wrapped.KeyID,
			WrappedDEK:    wrapped.WrappedKey,
			WrapAlgorithm: wrapped.WrapAlgorithm,
		}, ciphertext, true

	case string(encryption.StrictZK):
		algo := r.Header.Get("X-Amz-Meta-Zk-Encryption")
		if algo == "" {
			writeError(w, http.StatusForbidden, "EncryptionRequired",
				"tenant policy requires client_side encryption; set X-Amz-Meta-Zk-Encryption header", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, false
		}
		return metadata.EncryptionConfig{
			Mode:      encMode,
			Algorithm: algo,
		}, plaintext, true
	}

	return metadata.EncryptionConfig{}, plaintext, true
}
