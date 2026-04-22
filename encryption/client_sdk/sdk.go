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
package client_sdk

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
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
}

func (o Options) chunkSize() int {
	if o.ChunkSize > 0 {
		return o.ChunkSize
	}
	return DefaultChunkSize
}

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
	return &encryptReader{
		src:       plaintext,
		aead:      aead,
		chunkSize: opts.chunkSize(),
	}, nil
}

// DecryptObject is the mirror of EncryptObject: it walks chunk
// frames on ciphertext and returns a reader that yields the original
// plaintext.
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
	}, nil
}

// chunkHeaderSize is the bytes reserved at the head of every
// ciphertext chunk frame: the 24-byte XChaCha20-Poly1305 nonce plus
// the 4-byte big-endian ciphertext length that lets the decryptor
// compute frame boundaries without a separate manifest.
const chunkHeaderSize = chacha20poly1305.NonceSizeX + 4

// encryptReader streams ciphertext chunks on demand.
type encryptReader struct {
	src       io.Reader
	aead      cipherAEAD
	chunkSize int
	pending   bytes.Buffer
	eof       bool
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

	nonce := make([]byte, r.aead.NonceSize())
	if _, err := io.ReadFull(randReader, nonce); err != nil {
		return fmt.Errorf("client_sdk: generate nonce: %w", err)
	}
	sealed := r.aead.Seal(nil, nonce, buf[:n], nil)

	var hdr [chunkHeaderSize]byte
	copy(hdr[:r.aead.NonceSize()], nonce)
	binary.BigEndian.PutUint32(hdr[r.aead.NonceSize():], uint32(len(sealed)))
	r.pending.Write(hdr[:])
	r.pending.Write(sealed)
	return nil
}

// decryptReader streams plaintext chunks on demand.
type decryptReader struct {
	src       io.Reader
	aead      cipherAEAD
	chunkSize int
	pending   bytes.Buffer
	eof       bool
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
	pt, err := r.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return fmt.Errorf("client_sdk: open frame: %w", err)
	}
	r.pending.Write(pt)
	if len(pt) < r.chunkSize {
		r.eof = true
	}
	return nil
}
