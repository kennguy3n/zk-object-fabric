package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

func TestNew_RejectsInvalidTable(t *testing.T) {
	_, err := New(Config{DB: nil, Table: "manifests"})
	if err == nil {
		t.Error("New with nil DB should error")
	}
}

func TestIsSafeIdent(t *testing.T) {
	good := []string{"manifests", "Manifests", "manifests_v2", "_private", "a1"}
	bad := []string{"", "1bad", "ma-nifests", "manifests ", "manifests;--", "\"manifests\""}
	for _, s := range good {
		if !isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if isSafeIdent(s) {
			t.Errorf("isSafeIdent(%q) = true, want false", s)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		hash, version string
	}{
		{"", ""},
		{"abc", "v1"},
		{"hash_with_underscores", "version_id"},
	} {
		enc := joinCursor(tc.hash, tc.version)
		gotHash, gotVersion := splitCursor(enc)
		if gotHash != tc.hash || gotVersion != tc.version {
			t.Errorf("cursor roundtrip %q/%q -> %q -> %q/%q", tc.hash, tc.version, enc, gotHash, gotVersion)
		}
	}
}

func TestValidateKey(t *testing.T) {
	cases := []struct {
		name string
		key  manifest_store.ManifestKey
		ok   bool
	}{
		{"full", manifest_store.ManifestKey{TenantID: "t", Bucket: "b", ObjectKeyHash: "h", VersionID: "v"}, true},
		{"no tenant", manifest_store.ManifestKey{Bucket: "b", ObjectKeyHash: "h", VersionID: "v"}, false},
		{"no bucket", manifest_store.ManifestKey{TenantID: "t", ObjectKeyHash: "h", VersionID: "v"}, false},
		{"no hash", manifest_store.ManifestKey{TenantID: "t", Bucket: "b", VersionID: "v"}, false},
		{"no version", manifest_store.ManifestKey{TenantID: "t", Bucket: "b", ObjectKeyHash: "h"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateKey(tc.key)
			if tc.ok && err != nil {
				t.Errorf("validateKey(%+v) = %v, want nil", tc.key, err)
			}
			if !tc.ok && err == nil {
				t.Errorf("validateKey(%+v) = nil, want error", tc.key)
			}
		})
	}
}

// TestStoreImplementsInterface is a compile-time sanity check that the
// exported struct satisfies the interface it claims to.
func TestStoreImplementsInterface(t *testing.T) {
	var _ manifest_store.ManifestStore = (*Store)(nil)
}

// Exercise one end-to-end code path without a real DB by using the
// nil-DB guard rather than a full sql.DB mock.
func TestGet_RejectsEmptyKeyFields(t *testing.T) {
	s := &Store{table: "manifests"}
	_, err := s.Get(context.Background(), manifest_store.ManifestKey{})
	if err == nil {
		t.Error("Get with empty key should error")
	}
	if errors.Is(err, manifest_store.ErrNotFound) {
		t.Error("Get with empty key should not return ErrNotFound")
	}
}
