package billing

import (
	"bytes"
	"context"
	"log"
	"strings"
	"testing"
)

func TestNoopProvider_EnsureCustomer(t *testing.T) {
	p := NewNoopProvider(nil)
	h, err := p.EnsureCustomer(context.Background(), CustomerRequest{
		TenantID: "tenant-a",
		Email:    "ops@example.com",
		Name:     "Acme",
	})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if h.Provider != "noop" {
		t.Fatalf("Provider = %q, want \"noop\"", h.Provider)
	}
	if !strings.HasPrefix(h.ProviderRef, "noop_cus_") {
		t.Fatalf("ProviderRef = %q, want noop_cus_*", h.ProviderRef)
	}
}

func TestNoopProvider_EnsureCustomer_RequiresTenantID(t *testing.T) {
	p := NewNoopProvider(nil)
	if _, err := p.EnsureCustomer(context.Background(), CustomerRequest{}); err == nil {
		t.Fatalf("expected error for empty tenant_id")
	}
}

func TestNoopProvider_FullLifecycle(t *testing.T) {
	var buf bytes.Buffer
	p := NewNoopProvider(log.New(&buf, "", 0))

	cust, err := p.EnsureCustomer(context.Background(), CustomerRequest{TenantID: "t1", Email: "x@y"})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	sub, err := p.EnsureSubscription(context.Background(), SubscriptionRequest{
		TenantID:    "t1",
		CustomerRef: cust.ProviderRef,
		PlanID:      "plan_pro",
	})
	if err != nil {
		t.Fatalf("EnsureSubscription: %v", err)
	}
	if sub.Status != "active" {
		t.Fatalf("Status = %q, want active", sub.Status)
	}

	if err := p.ReportUsage(context.Background(), []UsageEvent{{TenantID: "t1", Dimension: PutRequests, Delta: 5}}); err != nil {
		t.Fatalf("ReportUsage: %v", err)
	}

	inv, err := p.IssueInvoice(context.Background(), InvoiceRequest{
		TenantID:    "t1",
		CustomerRef: cust.ProviderRef,
		LineItems:   []InvoiceLineItem{{Description: "setup fee", AmountMinor: 5_000_00, Currency: "USD", Quantity: 1}},
	})
	if err != nil {
		t.Fatalf("IssueInvoice: %v", err)
	}
	if inv.ProviderRef == "" {
		t.Fatalf("expected non-empty ProviderRef")
	}

	if err := p.CancelSubscription(context.Background(), sub.ProviderRef); err != nil {
		t.Fatalf("CancelSubscription: %v", err)
	}

	out := buf.String()
	for _, want := range []string{
		"billing.noop: ensure_customer tenant=t1",
		"billing.noop: ensure_subscription tenant=t1",
		"billing.noop: report_usage events=1",
		"billing.noop: issue_invoice tenant=t1",
		"billing.noop: cancel_subscription",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\nfull log:\n%s", want, out)
		}
	}
}

func TestNoopProvider_CustomIDGenerator(t *testing.T) {
	p := &NoopProvider{IDGenerator: func(prefix string) string { return prefix + ":fixed" }}
	h, err := p.EnsureCustomer(context.Background(), CustomerRequest{TenantID: "t"})
	if err != nil {
		t.Fatalf("EnsureCustomer: %v", err)
	}
	if h.ProviderRef != "noop_cus:fixed" {
		t.Fatalf("ProviderRef = %q, want noop_cus:fixed", h.ProviderRef)
	}
}
