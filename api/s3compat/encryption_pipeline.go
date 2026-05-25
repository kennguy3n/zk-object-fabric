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
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
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
	// DEK scrubbing: zero the raw DEK now that both EncryptObject
	// (which holds an independent AEAD key schedule in chacha20poly1305)
	// and WrapDEK (which has produced the wrapped form recorded on
	// the manifest) have consumed it. The SDK's EncryptObject
	// always copies the DEK into the encryptReader before returning
	// (see encryption/client_sdk/sdk.go: EncryptObject), so the
	// caller's backing array is safe to clear in both random-nonce
	// and convergent-nonce modes without corrupting the in-flight
	// stream. Mirrors the plaintext scrubbing pattern applied at
	// the call sites; closes the gap noted in PR #74 review.
	clear(dek)
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
	encReader, err := client_sdk.EncryptObject(plaintext, dek, client_sdk.Options{})
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, fmt.Errorf("s3compat: wrap dek: %w", err)
	}
	// DEK scrubbing: zero the raw DEK now that EncryptObject's AEAD
	// has stashed an independent chacha20poly1305 key schedule and
	// WrapDEK has produced the manifest-bound wrapped form. The
	// SDK's EncryptObject always copies the DEK into the
	// encryptReader before returning, so clearing the caller's
	// backing array does not corrupt the in-flight stream. Mirrors
	// the plaintext scrubbing pattern already in place at the call
	// sites; closes the gap noted in PR #74 review.
	clear(dek)
	return encReader, wrapped, nil
}

// encryptWithDEK seals plaintext with an already-generated DEK. It
// is used by the multipart path so every part of a single upload
// shares the same key: the DEK is generated at CreateMultipartUpload
// time, wrapped once, and then handed to every UploadPart call.
func (h *Handler) encryptWithDEK(plaintext []byte, dek client_sdk.DataEncryptionKey) ([]byte, error) {
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
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
	return h.decryptWithDEK(ciphertext, dek)
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
	return decReader, nil
}

// prepareSinglePieceEncryption consumes r.Body, applies the
// encryption mode dictated by the tenant's policy, and returns the
// body reader the gateway should hand to the storage backend, its
// content length, the plaintext size (for manifest.ObjectSize), and
// the EncryptionConfig to record on the manifest. A false second
// return indicates the helper already wrote a response and the
// caller should return.
//
//   - managed / public_distribution: the body is read in full,
//     encrypted with a fresh DEK, wrapped with the gateway's CMK,
//     and handed to the backend as ciphertext.
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
) (metadata.EncryptionConfig, io.Reader, int64, int64, bool) {
	switch encMode {
	case string(encryption.ManagedEncrypted), string(encryption.PublicDistribution):
		if h.cfg.Encryption == nil {
			writeError(w, http.StatusInternalServerError, "EncryptionNotConfigured",
				"tenant policy requires managed encryption but no gateway encryption is configured", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, 0, false
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
			encReader, wrapped, err := h.streamEncryptForStorage(r.Body)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
				return metadata.EncryptionConfig{}, nil, 0, 0, false
			}
			cfg := metadata.EncryptionConfig{
				Mode:          encMode,
				Algorithm:     client_sdk.ContentAlgorithm,
				KeyID:         wrapped.KeyID,
				WrappedDEK:    wrapped.WrappedKey,
				WrapAlgorithm: wrapped.WrapAlgorithm,
			}
			ciphertextSize := client_sdk.EncryptedSize(plaintextSize, client_sdk.Options{})
			return cfg, encReader, ciphertextSize, plaintextSize, true
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
			return metadata.EncryptionConfig{}, nil, 0, 0, false
		}
		ciphertext, wrapped, err := h.encryptForStorage(plaintext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, 0, false
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
		return cfg, bytes.NewReader(ciphertext), int64(len(ciphertext)), plaintextSize, true

	case string(encryption.StrictZK):
		algo := r.Header.Get("X-Amz-Meta-Zk-Encryption")
		if algo == "" {
			writeError(w, http.StatusForbidden, "EncryptionRequired",
				"tenant policy requires client_side encryption; set X-Amz-Meta-Zk-Encryption header", r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, 0, false
		}
		// Pass the client's ciphertext through unchanged. The
		// gateway does not read it, does not own the DEK, and
		// does not record wrapping material on the manifest.
		cfg := metadata.EncryptionConfig{
			Mode:      encMode,
			Algorithm: algo,
		}
		return cfg, r.Body, r.ContentLength, r.ContentLength, true
	}

	// Empty mode = legacy / no encryption. The body is passed
	// through verbatim and manifest.Encryption stays zero-valued.
	return metadata.EncryptionConfig{}, r.Body, r.ContentLength, r.ContentLength, true
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
