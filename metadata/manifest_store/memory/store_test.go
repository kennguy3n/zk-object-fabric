package memory

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

// TestCloneManifest_DeepClonesPolicyPointers pins the contract
// that the memory store treats stored manifests as immutable.
//
// Devin Review on PR #79 flagged that cloneManifest did a
// shallow `cp := *m` and only deep-cloned the slice fields
// (Pieces, Residency, AllowedBackends). The pointer-typed policy
// fields nested under PlacementPolicy (ReplicationPolicy,
// DedupPolicy) were left aliasing the caller's pointees, so a
// caller that mutated the policy struct after Put would see the
// mutation reflected in subsequent Gets — breaking the
// reference-contract that the Postgres store provides via SQL
// round-trip serialisation.
//
// The test does the full round trip: PUT a manifest with both
// policy pointers populated, mutate the caller's policy struct
// after the Put returns, then GET the manifest back and assert
// the stored copy still carries the original policy values.
func TestCloneManifest_DeepClonesPolicyPointers(t *testing.T) {
	t.Parallel()

	store := New()
	ctx := context.Background()

	rp := &metadata.ReplicationPolicy{
		SourceCell: "us-east-1",
		DestCell:   "us-west-2",
		Mode:       "async",
		RPO:        "60s",
	}
	dp := &metadata.DedupPolicy{
		Enabled: true,
		Scope:   "intra_tenant",
		Level:   "object",
	}

	manifest := &metadata.ObjectManifest{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKey:     "k1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
		PlacementPolicy: metadata.PlacementPolicy{
			ReplicationPolicy: rp,
			DedupPolicy:       dp,
		},
	}
	key := manifest_store.ManifestKey{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
	}

	if err := store.Put(ctx, key, manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Mutate the caller's pointees AFTER the Put. The memory
	// store's stored clone must not observe these mutations.
	rp.SourceCell = "MUTATED-source"
	rp.DestCell = "MUTATED-dest"
	rp.Mode = "MUTATED-mode"
	rp.RPO = "MUTATED-rpo"
	dp.Enabled = false
	dp.Scope = "MUTATED-scope"
	dp.Level = "MUTATED-level"

	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlacementPolicy.ReplicationPolicy == nil {
		t.Fatal("ReplicationPolicy lost across Put/Get")
	}
	if got.PlacementPolicy.ReplicationPolicy == rp {
		t.Fatal("ReplicationPolicy pointer was not cloned — Get returned the caller's pointee")
	}
	if got.PlacementPolicy.ReplicationPolicy.SourceCell != "us-east-1" {
		t.Errorf("ReplicationPolicy.SourceCell = %q, want %q (post-Put caller mutation leaked into stored copy)",
			got.PlacementPolicy.ReplicationPolicy.SourceCell, "us-east-1")
	}
	if got.PlacementPolicy.ReplicationPolicy.DestCell != "us-west-2" {
		t.Errorf("ReplicationPolicy.DestCell = %q, want %q", got.PlacementPolicy.ReplicationPolicy.DestCell, "us-west-2")
	}
	if got.PlacementPolicy.ReplicationPolicy.Mode != "async" {
		t.Errorf("ReplicationPolicy.Mode = %q, want %q", got.PlacementPolicy.ReplicationPolicy.Mode, "async")
	}
	if got.PlacementPolicy.ReplicationPolicy.RPO != "60s" {
		t.Errorf("ReplicationPolicy.RPO = %q, want %q", got.PlacementPolicy.ReplicationPolicy.RPO, "60s")
	}
	if got.PlacementPolicy.DedupPolicy == nil {
		t.Fatal("DedupPolicy lost across Put/Get")
	}
	if got.PlacementPolicy.DedupPolicy == dp {
		t.Fatal("DedupPolicy pointer was not cloned — Get returned the caller's pointee")
	}
	if !got.PlacementPolicy.DedupPolicy.Enabled {
		t.Errorf("DedupPolicy.Enabled = false, want true (caller mutation leaked into stored copy)")
	}
	if got.PlacementPolicy.DedupPolicy.Scope != "intra_tenant" {
		t.Errorf("DedupPolicy.Scope = %q, want %q", got.PlacementPolicy.DedupPolicy.Scope, "intra_tenant")
	}
	if got.PlacementPolicy.DedupPolicy.Level != "object" {
		t.Errorf("DedupPolicy.Level = %q, want %q", got.PlacementPolicy.DedupPolicy.Level, "object")
	}

	// The inverse path: the returned manifest's policy structs
	// must not alias the store's stored copy either. Mutating
	// the Get-returned policies must not affect subsequent Gets.
	got.PlacementPolicy.ReplicationPolicy.SourceCell = "GET-MUTATION"
	got.PlacementPolicy.DedupPolicy.Level = "GET-MUTATION"

	got2, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get (re-read): %v", err)
	}
	if got2.PlacementPolicy.ReplicationPolicy.SourceCell != "us-east-1" {
		t.Errorf("ReplicationPolicy.SourceCell after Get-side mutation = %q, want %q",
			got2.PlacementPolicy.ReplicationPolicy.SourceCell, "us-east-1")
	}
	if got2.PlacementPolicy.DedupPolicy.Level != "object" {
		t.Errorf("DedupPolicy.Level after Get-side mutation = %q, want %q",
			got2.PlacementPolicy.DedupPolicy.Level, "object")
	}
}

// TestCloneManifest_NilPolicyPointersStayNil pins that the
// deep-clone path does not turn a legitimately-nil policy into a
// non-nil zero-valued one. The omitempty JSON tag on both fields
// depends on nil == "no policy", so a clone that materialises an
// empty struct would corrupt the serialised representation.
func TestCloneManifest_NilPolicyPointersStayNil(t *testing.T) {
	t.Parallel()

	store := New()
	ctx := context.Background()
	key := manifest_store.ManifestKey{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
	}
	manifest := &metadata.ObjectManifest{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKey:     "k1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
		// PlacementPolicy left zero-valued; both pointer fields
		// inside it should remain nil after the clone.
	}

	if err := store.Put(ctx, key, manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.PlacementPolicy.ReplicationPolicy != nil {
		t.Errorf("ReplicationPolicy = %+v, want nil", got.PlacementPolicy.ReplicationPolicy)
	}
	if got.PlacementPolicy.DedupPolicy != nil {
		t.Errorf("DedupPolicy = %+v, want nil", got.PlacementPolicy.DedupPolicy)
	}
}

// TestCloneManifest_DeepClonesWrappedDEK pins that the
// EncryptionConfig.WrappedDEK byte slice is deep-cloned across
// Put/Get round-trips.
//
// Devin Review on PR #79 flagged that the original deep-clone
// fix only covered the pointer fields under PlacementPolicy and
// missed EncryptionConfig.WrappedDEK — a []byte that the shallow
// `cp := *m` copies as a slice header still aliasing the
// source's backing array. A caller that mutates the DEK bytes
// after Put would corrupt the stored copy and (worse) any
// concurrent reader's Get-returned copy, since both share the
// backing array. WrappedDEK is freshly allocated on the production
// PUT path so the aliasing window is narrow in practice, but the
// "stored manifests are immutable once Put-ed" invariant must
// hold byte-for-byte for security-sensitive code paths.
//
// The test does the full bidirectional round trip:
//
//  1. PUT a manifest with WrappedDEK populated.
//  2. Mutate the caller's DEK bytes after Put.
//  3. GET back and assert the stored DEK is still the original
//     bytes, AND the GET-returned slice is a different backing
//     array (not the caller's, not the store's).
//  4. Mutate the GET-returned slice and re-GET; the stored copy
//     must still hold the original bytes.
func TestCloneManifest_DeepClonesWrappedDEK(t *testing.T) {
	t.Parallel()

	store := New()
	ctx := context.Background()
	key := manifest_store.ManifestKey{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
	}
	originalDEK := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	callerDEK := append([]byte(nil), originalDEK...)

	manifest := &metadata.ObjectManifest{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKey:     "k1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
		Encryption: metadata.EncryptionConfig{
			Mode:          "managed",
			Algorithm:     "xchacha20-poly1305",
			KeyID:         "kms-key-1",
			WrappedDEK:    callerDEK,
			WrapAlgorithm: "aes-256-gcm",
		},
	}

	if err := store.Put(ctx, key, manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Mutate the caller's DEK bytes AFTER the Put. The store's
	// stored clone must not observe these mutations.
	for i := range callerDEK {
		callerDEK[i] = 0xFF
	}

	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got.Encryption.WrappedDEK, originalDEK) {
		t.Errorf("WrappedDEK after caller-side mutation = %x, want %x (caller mutation leaked into stored copy)",
			got.Encryption.WrappedDEK, originalDEK)
	}
	// The GET-returned slice must not share a backing array with
	// the caller's mutated slice. Comparing the first byte of each
	// is a sufficient check because the caller-side loop above
	// overwrote every byte to 0xFF.
	if len(got.Encryption.WrappedDEK) > 0 && len(callerDEK) > 0 &&
		&got.Encryption.WrappedDEK[0] == &callerDEK[0] {
		t.Fatal("WrappedDEK shares backing array with caller's slice — clone is shallow")
	}

	// The inverse path: mutate the GET-returned DEK and re-GET.
	// The stored copy must still hold the original bytes.
	for i := range got.Encryption.WrappedDEK {
		got.Encryption.WrappedDEK[i] = 0xAA
	}
	got2, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get (re-read): %v", err)
	}
	if !bytes.Equal(got2.Encryption.WrappedDEK, originalDEK) {
		t.Errorf("WrappedDEK after Get-side mutation = %x, want %x (Get-returned slice aliases stored copy)",
			got2.Encryption.WrappedDEK, originalDEK)
	}
}

// TestCloneManifest_NilWrappedDEKStaysNil pins that the
// deep-clone path does not turn a legitimately-nil WrappedDEK
// into a non-nil empty slice. The `omitempty` JSON tag on
// WrappedDEK depends on nil == "no DEK" (i.e. client-side
// encryption where the gateway never sees the key), so a clone
// that materialised an empty slice would corrupt the serialised
// representation.
func TestCloneManifest_NilWrappedDEKStaysNil(t *testing.T) {
	t.Parallel()

	store := New()
	ctx := context.Background()
	key := manifest_store.ManifestKey{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
	}
	manifest := &metadata.ObjectManifest{
		TenantID:      "t1",
		Bucket:        "b1",
		ObjectKey:     "k1",
		ObjectKeyHash: "h1",
		VersionID:     "v1",
		Encryption: metadata.EncryptionConfig{
			Mode:      "client_side",
			Algorithm: "xchacha20-poly1305",
			// WrappedDEK left nil: client holds the DEK.
		},
	}

	if err := store.Put(ctx, key, manifest); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := store.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Encryption.WrappedDEK != nil {
		t.Errorf("WrappedDEK = %x, want nil (client_side mode must not materialise an empty DEK)",
			got.Encryption.WrappedDEK)
	}
}

// TestScanManifests_PaginatesEveryVersionInKeyOrder verifies that
// ScanManifests visits every manifest version (not just the latest)
// across all tenants and buckets exactly once, in full-primary-key
// order, and that the cursor resumes without gaps or repeats.
func TestScanManifests_PaginatesEveryVersionInKeyOrder(t *testing.T) {
	t.Parallel()
	store := New()
	ctx := context.Background()

	// Seed two tenants, two buckets, and multiple versions per
	// object so the scan must cross every key boundary and return
	// older versions List would hide.
	want := map[manifest_store.ManifestKey]bool{}
	seed := func(tenant, bucket, key, version string) {
		mk := manifest_store.ManifestKey{TenantID: tenant, Bucket: bucket, ObjectKeyHash: key, VersionID: version}
		if err := store.Put(ctx, mk, &metadata.ObjectManifest{
			TenantID: tenant, Bucket: bucket, ObjectKeyHash: key, VersionID: version,
		}); err != nil {
			t.Fatalf("seed %v: %v", mk, err)
		}
		want[mk] = true
	}
	seed("t1", "b1", "h1", "v1")
	seed("t1", "b1", "h1", "v2") // older version of same object
	seed("t1", "b1", "h2", "v1")
	seed("t1", "b2", "h1", "v1")
	seed("t2", "b1", "h1", "v1")

	got := map[manifest_store.ManifestKey]int{}
	var prev manifest_store.ManifestKey
	var havePrev bool
	cursor := ""
	pages := 0
	for {
		page, err := store.ScanManifests(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("ScanManifests: %v", err)
		}
		pages++
		for _, sm := range page.Manifests {
			got[sm.Key]++
			if havePrev && !scanKeyLess(prev, sm.Key) {
				t.Fatalf("scan not strictly increasing: %v then %v", prev, sm.Key)
			}
			prev, havePrev = sm.Key, true
			if sm.Manifest == nil {
				t.Fatalf("nil manifest for %v", sm.Key)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("scanned %d distinct keys, want %d", len(got), len(want))
	}
	for k := range want {
		if got[k] != 1 {
			t.Fatalf("key %v visited %d times, want exactly 1", k, got[k])
		}
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages with limit=2 over %d keys, got %d", len(want), pages)
	}
}

// TestScanManifests_RejectsMalformedCursor ensures a corrupted cursor
// is an error rather than a silent restart from the beginning.
func TestScanManifests_RejectsMalformedCursor(t *testing.T) {
	t.Parallel()
	store := New()
	if _, err := store.ScanManifests(context.Background(), "!!!not-base64!!!", 10); err == nil {
		t.Fatal("malformed cursor: want error, got nil")
	}
}

// TestUpdateManifest_PreservesLatestPointer pins the contract:
// amending a NON-latest version's body (e.g. tagging an old version)
// must not promote it to latest. After Put(v1), Put(v2),
// UpdateManifest(v1), the empty-VersionID "latest" read must still
// resolve to v2.
func TestUpdateManifest_PreservesLatestPointer(t *testing.T) {
	t.Parallel()
	store := New()
	ctx := context.Background()

	base := manifest_store.ManifestKey{TenantID: "t1", Bucket: "b1", ObjectKeyHash: "h1"}
	kv1 := base
	kv1.VersionID = "v1"
	kv2 := base
	kv2.VersionID = "v2"

	if err := store.Put(ctx, kv1, &metadata.ObjectManifest{TenantID: "t1", Bucket: "b1", VersionID: "v1"}); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := store.Put(ctx, kv2, &metadata.ObjectManifest{TenantID: "t1", Bucket: "b1", VersionID: "v2"}); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

	// Amend the OLD version v1 in place.
	if err := store.UpdateManifest(ctx, kv1, &metadata.ObjectManifest{
		TenantID: "t1", Bucket: "b1", VersionID: "v1",
		Tags: map[string]string{"env": "prod"},
	}); err != nil {
		t.Fatalf("UpdateManifest v1: %v", err)
	}

	// Latest must still be v2.
	latest, err := store.Get(ctx, base)
	if err != nil {
		t.Fatalf("Get latest: %v", err)
	}
	if latest.VersionID != "v2" {
		t.Fatalf("latest VersionID = %q, want v2 (UpdateManifest must not promote v1)", latest.VersionID)
	}
	if len(latest.Tags) != 0 {
		t.Fatalf("latest Tags = %v, want none", latest.Tags)
	}

	// And v1's body was actually amended.
	got1, err := store.Get(ctx, kv1)
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if got1.Tags["env"] != "prod" {
		t.Fatalf("v1 Tags = %v, want env=prod", got1.Tags)
	}
}

// TestUpdateManifest_NotFound verifies an amend to a non-existent
// version returns ErrNotFound rather than silently inserting.
func TestUpdateManifest_NotFound(t *testing.T) {
	t.Parallel()
	store := New()
	key := manifest_store.ManifestKey{TenantID: "t1", Bucket: "b1", ObjectKeyHash: "h1", VersionID: "nope"}
	err := store.UpdateManifest(context.Background(), key, &metadata.ObjectManifest{VersionID: "nope"})
	if !errors.Is(err, manifest_store.ErrNotFound) {
		t.Fatalf("UpdateManifest missing key err = %v, want ErrNotFound", err)
	}
}
