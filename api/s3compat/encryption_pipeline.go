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
// returns (ciphertext, wrapped DEK, plaintext DEK, error). The
// wrapped DEK is what the caller stores on the manifest; the
// plaintext DEK is returned for callers (multipart) that need to
// keep it in memory across a sequence of encrypt calls using the
// same key.
func (h *Handler) encryptForStorage(plaintext []byte) ([]byte, client_sdk.WrappedDEK, client_sdk.DataEncryptionKey, error) {
	if h.cfg.Encryption == nil {
		return nil, client_sdk.WrappedDEK{}, nil, fmt.Errorf("s3compat: gateway encryption is not configured")
	}
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, nil, fmt.Errorf("s3compat: generate dek: %w", err)
	}
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, nil, fmt.Errorf("s3compat: encrypt object: %w", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, nil, fmt.Errorf("s3compat: read ciphertext: %w", err)
	}
	wrapped, err := h.cfg.Encryption.Wrapper.WrapDEK(dek, h.cfg.Encryption.CMK)
	if err != nil {
		return nil, client_sdk.WrappedDEK{}, nil, fmt.Errorf("s3compat: wrap dek: %w", err)
	}
	return ciphertext, wrapped, dek, nil
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
		plaintext, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "InvalidArgument", "read body: "+err.Error(), r.URL.Path)
			return metadata.EncryptionConfig{}, nil, 0, 0, false
		}
		ciphertext, wrapped, _, err := h.encryptForStorage(plaintext)
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
		return cfg, bytes.NewReader(ciphertext), int64(len(ciphertext)), int64(len(plaintext)), true

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
		ciphertext, wrapped, _, err := h.encryptForStorage(plaintext)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "EncryptionFailed", err.Error(), r.URL.Path)
			return metadata.EncryptionConfig{}, nil, false
		}
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
