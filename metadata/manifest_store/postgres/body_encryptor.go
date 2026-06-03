// Manifest body encryption for the manifest stores.
//
// The concrete implementation now lives in the parent
// manifest_store package so the Postgres and SQLite backends share
// one copy of the crypto. This file keeps the historical
// postgres-package names working as aliases so existing callers
// (cmd/gateway, tests) compile unchanged.
package postgres

import (
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// BodyContext binds a manifest body to a specific manifest key.
// Alias of manifest_store.BodyContext.
type BodyContext = manifest_store.BodyContext

// BodyEncryptor seals and opens the manifest JSON document.
// Alias of manifest_store.BodyEncryptor.
type BodyEncryptor = manifest_store.BodyEncryptor

// AEADBodyEncryptor is the XChaCha20-Poly1305 BodyEncryptor.
// Alias of manifest_store.AEADBodyEncryptor.
type AEADBodyEncryptor = manifest_store.AEADBodyEncryptor

// NewAEADBodyEncryptor returns an encryptor keyed off the given 32
// bytes. It forwards to manifest_store.NewAEADBodyEncryptor.
func NewAEADBodyEncryptor(key []byte) (*AEADBodyEncryptor, error) {
	return manifest_store.NewAEADBodyEncryptor(key)
}
