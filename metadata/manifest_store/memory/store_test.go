package memory

import (
	"context"
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
