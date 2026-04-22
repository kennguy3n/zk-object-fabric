// Package providers_test wires the local_fs_dev adapter into the
// shared StorageProvider conformance suite defined in
// tests/providers/conformance.
//
// See wasabi_conformance_test.go for the same suite run against the
// Wasabi adapter (via an in-memory S3 fake so CI does not need real
// credentials).
package providers_test

import (
	"path/filepath"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	"github.com/kennguy3n/zk-object-fabric/tests/providers/conformance"
)

func TestStorageProvider_LocalFSDev(t *testing.T) {
	factory := func(t *testing.T) providers.StorageProvider {
		t.Helper()
		root := filepath.Join(t.TempDir(), "pieces")
		p, err := local_fs_dev.New(root)
		if err != nil {
			t.Fatalf("local_fs_dev.New: %v", err)
		}
		return p
	}
	conformance.Run(t, factory, conformance.Options{})
}
