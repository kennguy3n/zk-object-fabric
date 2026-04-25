package billing

import (
	"context"
	"strings"
	"testing"
)

func TestBuildProvider_NoopDefault(t *testing.T) {
	p, err := BuildProvider(ProviderFactoryConfig{})
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if p.Name() != "noop" {
		t.Fatalf("Name = %q, want noop", p.Name())
	}
}

func TestBuildProvider_UnknownName(t *testing.T) {
	_, err := BuildProvider(ProviderFactoryConfig{Name: "definitely-not-registered"})
	if err == nil {
		t.Fatalf("expected error for unknown provider")
	}
	if !strings.Contains(err.Error(), "no provider registered") {
		t.Fatalf("error = %q, want substring \"no provider registered\"", err.Error())
	}
}

func TestBuildProvider_CaseInsensitive(t *testing.T) {
	p, err := BuildProvider(ProviderFactoryConfig{Name: "NOOP"})
	if err != nil {
		t.Fatalf("BuildProvider NOOP: %v", err)
	}
	if p.Name() != "noop" {
		t.Fatalf("Name = %q, want noop", p.Name())
	}
}

func TestRegisterProvider_Duplicate(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on duplicate registration")
		}
	}()
	RegisterProvider("noop", func(_ ProviderFactoryConfig) (BillingProvider, error) {
		return NewNoopProvider(nil), nil
	})
}

func TestRegisterProvider_Custom(t *testing.T) {
	const name = "fake-test-provider"
	RegisterProvider(name, func(_ ProviderFactoryConfig) (BillingProvider, error) {
		return NewNoopProvider(nil), nil
	})
	defer func() {
		// no Unregister API by design; the test name is unique
		// so this leaks into other tests in this package
		// without conflict.
	}()
	p, err := BuildProvider(ProviderFactoryConfig{Name: name})
	if err != nil {
		t.Fatalf("BuildProvider: %v", err)
	}
	if _, err := p.EnsureCustomer(context.Background(), CustomerRequest{TenantID: "t1"}); err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	names := RegisteredProviders()
	found := false
	for _, n := range names {
		if n == name {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RegisteredProviders did not include %q: %v", name, names)
	}
}
