package auth

import (
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// TestMemoryTenantStore_CountTenants verifies that CountTenants counts
// distinct tenants — a tenant with multiple API-key bindings is
// counted once, and a tenant created without a binding still counts —
// in contrast to Size, which counts bindings.
func TestMemoryTenantStore_CountTenants(t *testing.T) {
	store := NewMemoryTenantStore()

	if got := store.CountTenants(); got != 0 {
		t.Fatalf("empty store: CountTenants() = %d, want 0", got)
	}

	// tenant-a holds two bindings; it must count once.
	for _, ak := range []string{"AKIA-A1", "AKIA-A2"} {
		if err := store.AddBinding(TenantBinding{
			AccessKey: ak,
			SecretKey: "secret",
			Tenant:    tenant.Tenant{ID: "tenant-a", Name: "Tenant A"},
		}); err != nil {
			t.Fatalf("AddBinding(%s): %v", ak, err)
		}
	}

	// tenant-b holds a single binding.
	if err := store.AddBinding(TenantBinding{
		AccessKey: "AKIA-B1",
		SecretKey: "secret",
		Tenant:    tenant.Tenant{ID: "tenant-b", Name: "Tenant B"},
	}); err != nil {
		t.Fatalf("AddBinding(AKIA-B1): %v", err)
	}

	// tenant-c is created but never bound; it must still count.
	if err := store.CreateTenant(tenant.Tenant{ID: "tenant-c", Name: "Tenant C"}); err != nil {
		t.Fatalf("CreateTenant(tenant-c): %v", err)
	}

	if got, want := store.Size(), 3; got != want {
		t.Errorf("Size() = %d, want %d (binding count)", got, want)
	}
	if got, want := store.CountTenants(), 3; got != want {
		t.Errorf("CountTenants() = %d, want %d (distinct tenants)", got, want)
	}
}
