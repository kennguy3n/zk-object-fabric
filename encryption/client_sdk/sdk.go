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
//	| 24-byte nonce | 4-byte BE ciphertext length | ciphertext (plaintext_len + 16-byte Poly1305 tag) |
//
// The 24+4 = 28-byte header (chunkHeaderSize) lets the decryptor
// compute the next frame boundary without a separate manifest. All
// chunks except the last carry exactly DefaultChunkSize bytes of
// plaintext; the last chunk may be shorter. Ciphertext chunks are
// self-describing — the decryptor walks frames sequentially using the
// length prefix and verifies each AEAD tag in place.
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
	"math"
	"math/bits"

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
	// Reject ConvergentNonce + ChunkAAD as a footgun. The two
	// options pursue *contradictory* goals and combining them
	// produces silently-broken output that *looks* successful:
	//
	//   * ConvergentNonce derives the per-chunk nonce from
	//     (DEK, chunkIndex) so that identical plaintext sealed
	//     under a content-derived DEK (see DeriveConvergentDEK
	//     in keygen.go) yields byte-identical ciphertext across
	//     tenants and uploads — the foundation of Pattern C
	//     cross-context client-side deduplication.
	//
	//   * ChunkAAD binds every chunk's Poly1305 tag to operator-
	//     supplied context (tenant_id, object_id, ...). Two
	//     callers with different ChunkAAD values, sealing the
	//     same plaintext under the same convergent DEK, produce
	//     identical *ciphertext bytes* (same key, same nonce,
	//     same plaintext) but *different* MAC tags. A storage
	//     backend that dedups on ciphertext bytes alone (which
	//     is the natural shape — tags are per-recipient) would
	//     point both tenants at the same physical block, then
	//     fail authentication for whichever tenant's tag was
	//     not the one persisted. Worse, if the backend dedups
	//     and stores only one tag, the *other* tenant's chunks
	//     become silently undecryptable on read-back even though
	//     EncryptObject reported success.
	//
	// The two flags cannot both be honoured. Rather than
	// silently winning one and breaking the other, refuse the
	// combination at the entry point so the operator is forced
	// to pick a single mode. ChunkAAD without ConvergentNonce
	// (the Strict-ZK / Pattern B path) and ConvergentNonce
	// without ChunkAAD (the convergent-dedup / Pattern C path)
	// are both fully supported.
	if opts.ConvergentNonce && len(opts.ChunkAAD) > 0 {
		return nil, errors.New("client_sdk: ConvergentNonce and ChunkAAD are mutually exclusive: " +
			"ConvergentNonce produces deterministic ciphertext for cross-tenant content dedup, " +
			"while ChunkAAD binds the per-chunk tag to operator-supplied context; combining " +
			"them yields identical ciphertext with diverging tags, which silently breaks " +
			"dedup or authentication depending on backend behaviour. Pick one: " +
			"ChunkAAD (Strict-ZK / Pattern B) OR ConvergentNonce (convergent / Pattern C)")
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

// chunkTrailerSize is the per-frame AEAD tag overhead appended by
// Seal. Together with chunkHeaderSize it is the deterministic
// per-frame on-disk cost the gateway uses to compute ciphertext
// length from plaintext length (see EncryptedSize).
//
// Bound to chacha20poly1305.Overhead (the Poly1305 authentication
// tag length) at compile time so any future swap of the AEAD
// algorithm in EncryptObject/DecryptObject cannot silently produce
// a wrong EncryptedSize prediction — the constant follows the
// actual AEAD's tag size in lockstep instead of being hand-rolled
// against a documented number.
const chunkTrailerSize = chacha20poly1305.Overhead

// ErrEncryptedSizeOverflow is returned by EncryptedSize when the
// predicted ciphertext size would overflow int64. The gateway PUT
// path must reject the request before calling Read on the encrypt
// reader — advertising a wrapped or zero ContentLength to the
// backend would either silently truncate the upload or cause the
// backend to drop the header and accept an arbitrary stream
// length. errors.Is(err, ErrEncryptedSizeOverflow) lets callers
// distinguish overflow from other input-validation failures.
//
// The sentinel exists so callers do NOT have to disambiguate the
// pre-refactor `EncryptedSize(...) == 0` overload (legitimate
// empty input vs overflow): empty input now returns (0, nil) and
// overflow returns (0, ErrEncryptedSizeOverflow).
var ErrEncryptedSizeOverflow = errors.New("client_sdk: ciphertext size overflows int64")

// ErrEncryptedSizeNegativePlaintext is returned by EncryptedSize
// for a negative plaintextLen. Negative lengths come from
// adversarial or malformed Content-Length headers; the gateway
// streaming PUT path is the only caller that ever passes
// untrusted plaintextLen values, and it should reject the request
// outright rather than silently fall back to "0".
var ErrEncryptedSizeNegativePlaintext = errors.New("client_sdk: negative plaintext length")

// ErrEncryptedSizeInvalidChunkSize is returned by EncryptedSize
// when Options.chunkSize() is non-positive. Should be unreachable
// in practice because Options.chunkSize() floors to
// DefaultChunkSize, but defending the contract makes future
// changes to Options safe.
var ErrEncryptedSizeInvalidChunkSize = errors.New("client_sdk: non-positive chunk size")

// EncryptedSize returns the exact ciphertext byte count that
// EncryptObject would emit for a plaintext of plaintextLen bytes
// sealed under the given Options. It is the inverse of decryptReader's
// frame walk: numChunks * (header + tag) + plaintextLen, where
// numChunks is ceil(plaintextLen / chunkSize) for non-empty input
// and 0 for empty input (no frame is emitted for a zero-length
// plaintext, matching encryptReader.nextFrame's EOF short-circuit).
//
// The gateway PUT path uses this to compute opts.ContentLength
// for backends that require a known length before the streaming
// SDK reader has produced any bytes.
//
// Error semantics:
//   - plaintextLen == 0  → (0, nil). Empty objects are legitimate;
//     no frame is emitted by EncryptObject.
//   - plaintextLen < 0   → (0, ErrEncryptedSizeNegativePlaintext).
//   - chunkSize <= 0     → (0, ErrEncryptedSizeInvalidChunkSize).
//   - overflow (the int64 ciphertext-size computation wraps for
//     a hostile Content-Length like math.MaxInt64) →
//     (0, ErrEncryptedSizeOverflow).
//
// Callers MUST check err != nil and reject the request before
// reading the encrypt stream. Pre-refactor the function returned
// a single int64 and used 0 as both "empty input" and "overflow"
// sentinel, which was a foot-gun for any future caller that
// forgot to add a plaintextLen > 0 disambiguation guard. The
// typed return forces every callsite to acknowledge the failure
// mode at the type level.
func EncryptedSize(plaintextLen int64, opts Options) (int64, error) {
	if plaintextLen < 0 {
		return 0, ErrEncryptedSizeNegativePlaintext
	}
	if plaintextLen == 0 {
		return 0, nil
	}
	chunk := int64(opts.chunkSize())
	if chunk <= 0 {
		return 0, ErrEncryptedSizeInvalidChunkSize
	}
	// numChunks = ceil(plaintextLen / chunk). Compute via
	// math/bits.Add64 so a near-MaxInt64 plaintextLen does not wrap
	// in the (plaintextLen + chunk - 1) intermediate.
	numeratorLo, carry := bits.Add64(uint64(plaintextLen), uint64(chunk-1), 0)
	if carry != 0 || numeratorLo > math.MaxInt64 {
		return 0, ErrEncryptedSizeOverflow
	}
	numChunks := int64(numeratorLo) / chunk
	// overhead = numChunks * (chunkHeaderSize + chunkTrailerSize).
	overheadHi, overheadLo := bits.Mul64(uint64(numChunks), uint64(chunkHeaderSize+chunkTrailerSize))
	if overheadHi != 0 || overheadLo > math.MaxInt64 {
		return 0, ErrEncryptedSizeOverflow
	}
	// total = plaintextLen + overhead.
	total, carry := bits.Add64(uint64(plaintextLen), overheadLo, 0)
	if carry != 0 || total > math.MaxInt64 {
		return 0, ErrEncryptedSizeOverflow
	}
	return int64(total), nil
}

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
	// Defence-in-depth plaintext scrubbing: zero the chunk
	// buffer once the AEAD has consumed it. A heap dump or
	// paged-out memory taken between Seal and GC would
	// otherwise reveal cleartext for one chunk per concurrent
	// encrypt stream.
	clear(buf[:n])

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
