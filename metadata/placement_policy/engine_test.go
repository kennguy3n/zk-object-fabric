package placement_policy

import (
	"context"
	"io"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeProvider is a minimal StorageProvider stand-in for engine
// tests. It only serves Capabilities / CostModel / PlacementLabels.
type fakeProvider struct {
	name    string
	cost    providers.ProviderCostModel
	labels  providers.PlacementLabels
}

func (f *fakeProvider) PutPiece(context.Context, string, io.Reader, providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{}, nil
}
func (f *fakeProvider) GetPiece(context.Context, string, *providers.ByteRange) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeProvider) HeadPiece(context.Context, string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, nil
}
func (f *fakeProvider) DeletePiece(context.Context, string) error { return nil }
func (f *fakeProvider) ListPieces(context.Context, string, string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}
func (f *fakeProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{SupportsRangeReads: true}
}
func (f *fakeProvider) CostModel() providers.ProviderCostModel  { return f.cost }
func (f *fakeProvider) PlacementLabels() providers.PlacementLabels { return f.labels }

func newProviderMap() map[string]providers.StorageProvider {
	return map[string]providers.StorageProvider{
		"wasabi": &fakeProvider{
			name:   "wasabi",
			cost:   providers.ProviderCostModel{StorageUSDPerTBMonth: 6.99, EgressUSDPerGB: 0.0},
			labels: providers.PlacementLabels{Provider: "wasabi", Region: "ap-southeast-1"},
		},
		"ceph_sg1": &fakeProvider{
			name:   "ceph_sg1",
			cost:   providers.ProviderCostModel{StorageUSDPerTBMonth: 10.0, EgressUSDPerGB: 0.0},
			labels: providers.PlacementLabels{Provider: "ceph_rgw", Region: "sg-1", Country: "SG"},
		},
		"customer_s3": &fakeProvider{
			name:   "customer_s3",
			cost:   providers.ProviderCostModel{StorageUSDPerTBMonth: 23.0, EgressUSDPerGB: 0.09},
			labels: providers.PlacementLabels{Provider: "aws_s3", Region: "us-east-1", Country: "US"},
		},
	}
}

func TestEngine_B2CPooled_PicksCheapest(t *testing.T) {
	e := NewEngine("wasabi", newProviderMap(), map[string]*Policy{
		"tenantA": {
			Tenant: "tenantA",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement:  PlacementSpec{Provider: []string{"wasabi", "ceph_sg1"}},
			},
		},
	})
	got, pol, err := e.ResolveBackend("tenantA", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "wasabi" {
		t.Fatalf("got backend %q, want wasabi (cheapest)", got)
	}
	if len(pol.AllowedBackends) != 2 {
		t.Fatalf("AllowedBackends = %v, want 2 entries", pol.AllowedBackends)
	}
}

func TestEngine_B2BDedicated_PinsToCephCell(t *testing.T) {
	e := NewEngine("wasabi", newProviderMap(), map[string]*Policy{
		"tenantSG": {
			Tenant: "tenantSG",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement: PlacementSpec{
					Provider: []string{"ceph_sg1"},
					Country:  []string{"SG"},
				},
			},
		},
	})
	got, pol, err := e.ResolveBackend("tenantSG", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "ceph_sg1" {
		t.Fatalf("got backend %q, want ceph_sg1", got)
	}
	if len(pol.Residency) != 1 || pol.Residency[0] != "SG" {
		t.Fatalf("Residency = %v, want [SG]", pol.Residency)
	}
}

func TestEngine_BYOC_PinsToCustomerBackend(t *testing.T) {
	e := NewEngine("wasabi", newProviderMap(), map[string]*Policy{
		"tenantBYOC": {
			Tenant: "tenantBYOC",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement: PlacementSpec{
					Provider: []string{"customer_s3"},
					Country:  []string{"US"},
				},
			},
		},
	})
	got, _, err := e.ResolveBackend("tenantBYOC", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "customer_s3" {
		t.Fatalf("got backend %q, want customer_s3", got)
	}
}

func TestEngine_FallsBackToDefault(t *testing.T) {
	e := NewEngine("wasabi", newProviderMap(), nil)
	got, _, err := e.ResolveBackend("unknown-tenant", "b", "k")
	if err != nil {
		t.Fatalf("ResolveBackend: %v", err)
	}
	if got != "wasabi" {
		t.Fatalf("got backend %q, want wasabi (default)", got)
	}
}

func TestEngine_NoEligibleBackend(t *testing.T) {
	e := NewEngine("wasabi", newProviderMap(), map[string]*Policy{
		"tenantJP": {
			Tenant: "tenantJP",
			Spec: PolicySpec{
				Encryption: EncryptionSpec{Mode: "client_side"},
				Placement:  PlacementSpec{Country: []string{"JP"}},
			},
		},
	})
	if _, _, err := e.ResolveBackend("tenantJP", "b", "k"); err == nil {
		t.Fatal("ResolveBackend: want error for unmatched constraints, got nil")
	}
}

func TestEngine_UnregisteredDefault(t *testing.T) {
	e := NewEngine("nonexistent", newProviderMap(), nil)
	if _, _, err := e.ResolveBackend("t", "b", "k"); err == nil {
		t.Fatal("ResolveBackend: want error for unregistered default, got nil")
	}
}
