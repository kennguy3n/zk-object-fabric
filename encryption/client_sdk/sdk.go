// Package client_sdk implements the Phase 2 client-side encryption
// SDK that underpins Strict ZK mode (docs/PROPOSAL.md §3.7).
//
// The SDK seals plaintext with XChaCha20-Poly1305 in fixed-size
// chunks so that range reads can decrypt individual chunks without
// reconstructing the entire object. Per-object DEKs are generated
// with crypto/rand (see keygen.go) and wrapped with the tenant's CMK
// (see wrap.go). Phase 2 uses a local key file as the CMK; Phase 3
// swaps in AWS KMS and Vault behind the same Wrapper interface.
//
// On-disk frame for a chunk is:
//
//	| 24-byte nonce | ciphertext (plaintext_len + 16-byte Poly1305 tag) |
//
// All chunks except the last carry exactly DefaultChunkSize bytes of
// plaintext. The last chunk may be shorter. Ciphertext chunks are
// self-describing — the decryptor walks frames sequentially — so the
// SDK does not need a separate manifest for framing metadata.
//
// # Additional Authenticated Data (AAD)
//
// Callers may bind per-chunk ciphertext to an object-level context
// by populating Options.ChunkAAD. When non-empty the AEAD AAD for
// every chunk is the concatenation
//
//	AAD = ChunkAAD || "|" || big-endian uint64(chunk_index)
//
// This prevents a frame from being lifted out of one object and
// replayed inside another (same DEK) — the chunk_index suffix also
// rejects within-object reordering. The recommended ChunkAAD
// payload is the canonical, pipe-separated tuple
//
//	tenant_id|bucket|object_key_hash|version_id
//
// where `object_key_hash` is a content-derived SHA-256 over the
// object key (so the binding survives a CompleteMultipartUpload
// that may reorder bucket / key formatting). Other SDK
// implementations MUST reproduce this format byte-for-byte to
// interoperate.
//
// When ChunkAAD is empty (zero-length slice) the SDK falls back to
// AAD = nil for both Seal and Open — this preserves ciphertext
// compatibility with Phase 1 / Phase 2 objects that were sealed
// before the AAD field existed.
package client_sdk

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// DefaultChunkSize is the plaintext chunk size used when a caller
// does not override it. 16 MiB balances range-read granularity
// against per-chunk auth overhead; it matches the Storj satellite
// default (studied, not vendored — see docs/STORAGE_INFRA.md).
const DefaultChunkSize = 16 * 1024 * 1024

// ContentAlgorithm is the canonical algorithm string recorded in
// encryption.DataEncryptionKey.Algorithm for SDK-sealed objects.
const ContentAlgorithm = "xchacha20-poly1305"

// DataEncryptionKey is the plaintext key used to seal chunks. It is
// never persisted in plaintext; the SDK carries it across EncryptObject
// / DecryptObject calls inside the same process and relies on wrap.go
// to hand the caller a WrappedDEK for storage.
type DataEncryptionKey []byte

// Options tunes EncryptObject / DecryptObject. The zero value uses
// DefaultChunkSize.
type Options struct {
	ChunkSize int

	// ConvergentNonce switches the SDK from random per-chunk
	// nonces to deterministic, content-derived nonces so that
	// identical plaintext (sealed under a convergent DEK from
	// DeriveConvergentDEK) produces identical ciphertext. Required
	// for Pattern C (client-side intra-tenant deduplication); see
	// docs/PROPOSAL.md §3.14. Trade-off: stored ciphertext loses
	// forward secrecy. Default false (random nonces, FS preserved).
	ConvergentNonce bool

	// ChunkAAD, when non-empty, is mixed into every per-chunk AEAD
	// AAD alongside the big-endian chunk index. See the package
	// doc for the canonical format. When empty the SDK seals every
	// chunk with AAD = nil so ciphertext stays compatible with
	// pre-AAD objects. EncryptObject and DecryptObject MUST be
	// invoked with the same ChunkAAD value or Open will return a
	// MAC failure.
	ChunkAAD []byte
}

func (o Options) chunkSize() int {
	if o.ChunkSize > 0 {
		return o.ChunkSize
	}
	return DefaultChunkSize
}

// ConvergentNonceInfo is the HKDF info prefix used when deriving
// per-chunk nonces in convergent-nonce mode (docs/PROPOSAL.md
// §3.14). Versioned so a future format break does not collide with
// existing manifests.
const ConvergentNonceInfo = "zkof-nonce-v1"

// EncryptObject returns a reader that yields the encrypted, chunk-
// framed form of plaintext. The caller reads the returned stream
// until EOF and writes the bytes to storage; nothing in the SDK
// buffers the full plaintext or ciphertext.
func EncryptObject(plaintext io.Reader, dek DataEncryptionKey, opts Options) (io.Reader, error) {
	if plaintext == nil {
		return nil, errors.New("client_sdk: plaintext is required")
	}
	aead, err := chacha20poly1305.NewX(dek)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: new xchacha20-poly1305: %w", err)
	}
	r := &encryptReader{
		src:       plaintext,
		aead:      aead,
		chunkSize: opts.chunkSize(),
		chunkAAD:  cloneNonEmpty(opts.ChunkAAD),
	}
	if opts.ConvergentNonce {
		// Hold a copy of the DEK on the reader so nextFrame can
		// derive deterministic per-chunk nonces. The DEK is
		// already in this process; copying does not extend its
		// exposure.
		r.convergent = true
		r.dek = append([]byte(nil), dek...)
	}
	return r, nil
}

// DecryptObject is the mirror of EncryptObject: it walks chunk
// frames on ciphertext and returns a reader that yields the original
// plaintext.
//
// Decrypt does not branch on ConvergentNonce — the on-disk frame
// format is identical (the nonce is stored in the frame header), so
// the decryptor reads the nonce off the wire either way. The flag
// is accepted for symmetry with EncryptObject.
func DecryptObject(ciphertext io.Reader, dek DataEncryptionKey, opts Options) (io.Reader, error) {
	if ciphertext == nil {
		return nil, errors.New("client_sdk: ciphertext is required")
	}
	aead, err := chacha20poly1305.NewX(dek)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: new xchacha20-poly1305: %w", err)
	}
	return &decryptReader{
		src:       ciphertext,
		aead:      aead,
		chunkSize: opts.chunkSize(),
		chunkAAD:  cloneNonEmpty(opts.ChunkAAD),
	}, nil
}

// cloneNonEmpty returns nil when b is empty so the SDK's AAD logic
// can branch cleanly on len(chunkAAD) == 0 without conflating a
// caller-passed empty slice with a nil slice.
func cloneNonEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return append([]byte(nil), b...)
}

// chunkAADBytes returns the AEAD AAD for the given chunkIndex. The
// returned slice is nil when chunkAAD is empty (legacy compat
// mode); otherwise it is the canonical pipe-separated form
// documented in the package doc:
//
//	chunkAAD || "|" || big-endian uint64(chunkIndex)
func chunkAADBytes(chunkAAD []byte, chunkIndex uint64) []byte {
	if len(chunkAAD) == 0 {
		return nil
	}
	aad := make([]byte, 0, len(chunkAAD)+1+8)
	aad = append(aad, chunkAAD...)
	aad = append(aad, '|')
	var idx [8]byte
	binary.BigEndian.PutUint64(idx[:], chunkIndex)
	aad = append(aad, idx[:]...)
	return aad
}

// deriveConvergentNonce returns the deterministic nonce used to
// seal chunkIndex in convergent-nonce mode. The derivation binds
// the nonce to (DEK, chunkIndex) so identical plaintext sealed
// under the same convergent DEK produces byte-identical ciphertext
// across uploads, while distinct chunk positions never collide
// inside the same object.
func deriveConvergentNonce(dek DataEncryptionKey, chunkIndex uint64, nonceSize int) ([]byte, error) {
	var idxBytes [8]byte
	binary.BigEndian.PutUint64(idxBytes[:], chunkIndex)
	info := append([]byte(ConvergentNonceInfo), idxBytes[:]...)
	r := hkdf.New(sha256.New, dek, nil, info)
	nonce := make([]byte, nonceSize)
	if _, err := io.ReadFull(r, nonce); err != nil {
		return nil, fmt.Errorf("client_sdk: derive convergent nonce: %w", err)
	}
	return nonce, nil
}

// chunkHeaderSize is the bytes reserved at the head of every
// ciphertext chunk frame: the 24-byte XChaCha20-Poly1305 nonce plus
// the 4-byte big-endian ciphertext length that lets the decryptor
// compute frame boundaries without a separate manifest.
const chunkHeaderSize = chacha20poly1305.NonceSizeX + 4

// encryptReader streams ciphertext chunks on demand.
type encryptReader struct {
	src        io.Reader
	aead       cipherAEAD
	chunkSize  int
	pending    bytes.Buffer
	eof        bool
	convergent bool
	dek        DataEncryptionKey
	chunkIndex uint64
	chunkAAD   []byte
}

// cipherAEAD is the subset of cipher.AEAD the SDK needs; exposed as
// an interface so tests can swap in a deterministic AEAD when
// exercising the chunking logic.
type cipherAEAD interface {
	NonceSize() int
	Overhead() int
	Seal(dst, nonce, plaintext, additionalData []byte) []byte
	Open(dst, nonce, ciphertext, additionalData []byte) ([]byte, error)
}

func (r *encryptReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 && !r.eof {
		if err := r.nextFrame(); err != nil {
			return 0, err
		}
	}
	n, err := r.pending.Read(p)
	if r.pending.Len() == 0 && r.eof && err == nil {
		err = io.EOF
	}
	return n, err
}

func (r *encryptReader) nextFrame() error {
	buf := make([]byte, r.chunkSize)
	n, err := io.ReadFull(r.src, buf)
	switch {
	case err == io.EOF:
		r.eof = true
		return nil
	case err == io.ErrUnexpectedEOF:
		r.eof = true
	case err != nil:
		return fmt.Errorf("client_sdk: read plaintext: %w", err)
	}

	nonceSize := r.aead.NonceSize()
	var nonce []byte
	if r.convergent {
		dn, derr := deriveConvergentNonce(r.dek, r.chunkIndex, nonceSize)
		if derr != nil {
			return derr
		}
		nonce = dn
	} else {
		nonce = make([]byte, nonceSize)
		if _, err := io.ReadFull(randReader, nonce); err != nil {
			return fmt.Errorf("client_sdk: generate nonce: %w", err)
		}
	}
	aad := chunkAADBytes(r.chunkAAD, r.chunkIndex)
	r.chunkIndex++
	sealed := r.aead.Seal(nil, nonce, buf[:n], aad)

	var hdr [chunkHeaderSize]byte
	copy(hdr[:nonceSize], nonce)
	binary.BigEndian.PutUint32(hdr[nonceSize:], uint32(len(sealed)))
	r.pending.Write(hdr[:])
	r.pending.Write(sealed)
	return nil
}

// decryptReader streams plaintext chunks on demand.
type decryptReader struct {
	src        io.Reader
	aead       cipherAEAD
	chunkSize  int
	pending    bytes.Buffer
	eof        bool
	chunkAAD   []byte
	chunkIndex uint64
}

func (r *decryptReader) Read(p []byte) (int, error) {
	for r.pending.Len() == 0 && !r.eof {
		if err := r.nextFrame(); err != nil {
			return 0, err
		}
	}
	n, err := r.pending.Read(p)
	if r.pending.Len() == 0 && r.eof && err == nil {
		err = io.EOF
	}
	return n, err
}

func (r *decryptReader) nextFrame() error {
	var hdr [chunkHeaderSize]byte
	_, err := io.ReadFull(r.src, hdr[:])
	switch {
	case err == io.EOF:
		r.eof = true
		return nil
	case err != nil:
		return fmt.Errorf("client_sdk: read frame header: %w", err)
	}

	nonceSize := r.aead.NonceSize()
	nonce := hdr[:nonceSize]
	ctLen := binary.BigEndian.Uint32(hdr[nonceSize:])
	maxCT := uint32(r.chunkSize + r.aead.Overhead())
	if ctLen == 0 || ctLen > maxCT {
		return fmt.Errorf("client_sdk: frame length %d out of bounds (max %d)", ctLen, maxCT)
	}

	ct := make([]byte, ctLen)
	if _, err := io.ReadFull(r.src, ct); err != nil {
		return fmt.Errorf("client_sdk: read frame body: %w", err)
	}
	aad := chunkAADBytes(r.chunkAAD, r.chunkIndex)
	r.chunkIndex++
	pt, err := r.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return fmt.Errorf("client_sdk: open frame: %w", err)
	}
	r.pending.Write(pt)
	if len(pt) < r.chunkSize {
		r.eof = true
	}
	return nil
}
