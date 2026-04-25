package billing

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"
)

// BillingProvider is the vendor-neutral abstraction the control
// plane uses to integrate with an external billing / invoicing
// system (Stripe, Chargebee, Recurly, an internal billing
// pipeline, …). It is intentionally separate from BillingSink:
//
//   - BillingSink ingests raw UsageEvent rows for metering /
//     analytics. It is required for every deployment because it
//     feeds the in-product abuse + analytics dashboards.
//   - BillingProvider is the optional outbound integration to a
//     payment / subscription provider that issues invoices and
//     charges customers. It is per-deployment and may be left
//     unset; the gateway then falls back to NoopProvider.
//
// Phase 3 ships only the abstraction and a no-op default. A
// future Phase 4 / 5 plug-in will register e.g. a StripeProvider
// behind this interface without any other code in the codebase
// needing to learn about Stripe specifically.
//
// Implementations must be safe for concurrent use.
type BillingProvider interface {
	// Name returns a stable, human-readable identifier for the
	// provider (e.g. "noop", "stripe", "chargebee"). Surfaced in
	// logs and the operator dashboard.
	Name() string

	// EnsureCustomer creates or looks up the provider-side
	// customer record bound to a tenant. The returned
	// CustomerHandle is opaque from the gateway's perspective —
	// only the provider interprets ProviderRef.
	EnsureCustomer(ctx context.Context, req CustomerRequest) (CustomerHandle, error)

	// EnsureSubscription creates or updates the subscription
	// (plan + price + metadata) bound to the customer. Idempotent
	// on (tenant_id, plan_id).
	EnsureSubscription(ctx context.Context, req SubscriptionRequest) (SubscriptionHandle, error)

	// ReportUsage forwards a batch of UsageEvent records to the
	// provider's metered-billing endpoint. The batch shape
	// mirrors the events the BillingSink ingests, but the
	// provider may aggregate / round / drop dimensions per its
	// own pricing model.
	ReportUsage(ctx context.Context, events []UsageEvent) error

	// IssueInvoice triggers an out-of-cycle invoice for a
	// customer (e.g. one-shot dedicated-cell setup fees). The
	// returned InvoiceHandle carries the provider's invoice
	// identifier so the console can deep-link to it.
	IssueInvoice(ctx context.Context, req InvoiceRequest) (InvoiceHandle, error)

	// CancelSubscription tears down a subscription (e.g. on
	// tenant deletion). Idempotent on already-cancelled
	// subscriptions.
	CancelSubscription(ctx context.Context, subscriptionID string) error
}

// CustomerRequest describes the tenant-side customer record the
// gateway wants reflected on the provider. Fields are
// vendor-neutral — concrete providers map them onto their own
// schemas.
type CustomerRequest struct {
	// TenantID is the gateway's stable tenant identifier.
	// Required.
	TenantID string

	// Email is the billing contact for the tenant. May be empty
	// for B2B contracts that bill out-of-band.
	Email string

	// Name is the customer-facing display name.
	Name string

	// Country is the ISO-3166-1 alpha-2 country code, used for
	// tax handling.
	Country string

	// Metadata is a free-form key/value map; providers should
	// round-trip it as-is.
	Metadata map[string]string
}

// CustomerHandle is the opaque result of EnsureCustomer. The
// gateway persists ProviderRef alongside its tenant record so
// subsequent calls can short-circuit the lookup.
type CustomerHandle struct {
	// Provider is the provider name (e.g. "stripe").
	Provider string
	// ProviderRef is the provider-side customer ID
	// (e.g. "cus_…"). Opaque to the gateway.
	ProviderRef string
}

// SubscriptionRequest describes the subscription to create or
// update for a tenant.
type SubscriptionRequest struct {
	TenantID string
	// CustomerRef links the subscription to a CustomerHandle
	// previously returned by EnsureCustomer.
	CustomerRef string
	// PlanID identifies the plan / price object on the
	// provider. Opaque to the gateway.
	PlanID string
	// TrialDays is the trial length in days. Zero disables
	// trials.
	TrialDays int
	// Metadata is a free-form key/value map.
	Metadata map[string]string
}

// SubscriptionHandle is the opaque result of
// EnsureSubscription.
type SubscriptionHandle struct {
	Provider    string
	ProviderRef string
	// Status is the provider-reported subscription status
	// (e.g. "active", "trialing"). Empty when the provider
	// does not surface a status.
	Status string
}

// InvoiceRequest describes a one-shot invoice to issue.
type InvoiceRequest struct {
	TenantID    string
	CustomerRef string
	// LineItems is the list of charges. Implementations are
	// free to coalesce identical items.
	LineItems []InvoiceLineItem
	// DueAt is the optional due date. Zero defers to the
	// provider's default net-N policy.
	DueAt time.Time
	// Metadata is a free-form key/value map.
	Metadata map[string]string
}

// InvoiceLineItem is a single charge on an invoice.
type InvoiceLineItem struct {
	Description string
	// AmountMinor is the charge in the smallest currency unit
	// (e.g. cents). Use minor units to avoid floating-point
	// rounding drift across providers.
	AmountMinor int64
	Currency    string
	Quantity    uint64
}

// InvoiceHandle is the opaque result of IssueInvoice.
type InvoiceHandle struct {
	Provider    string
	ProviderRef string
	// HostedURL, when non-empty, is the customer-facing URL
	// (e.g. Stripe-hosted invoice page) the console can
	// deep-link to.
	HostedURL string
}

// NoopProvider is the default zero-config provider. Every method
// succeeds and records a structured log line so deployments that
// have not yet wired a real billing provider get a working
// gateway and an audit trail of what would have been billed.
//
// NoopProvider is safe for concurrent use.
type NoopProvider struct {
	// Logger receives structured log lines. Nil disables
	// logging.
	Logger *log.Logger

	// IDGenerator mints noop-side handles. Defaults to a
	// monotonically-increasing in-memory counter.
	IDGenerator func(prefix string) string

	mu      sync.Mutex
	counter uint64
}

// NewNoopProvider returns a NoopProvider with stdout logging.
// Pass nil for logger to disable logging.
func NewNoopProvider(logger *log.Logger) *NoopProvider {
	return &NoopProvider{Logger: logger}
}

func (p *NoopProvider) Name() string { return "noop" }

func (p *NoopProvider) nextID(prefix string) string {
	if p.IDGenerator != nil {
		return p.IDGenerator(prefix)
	}
	p.mu.Lock()
	p.counter++
	id := p.counter
	p.mu.Unlock()
	return fmt.Sprintf("%s_%d", prefix, id)
}

func (p *NoopProvider) logf(format string, args ...interface{}) {
	if p.Logger == nil {
		return
	}
	p.Logger.Printf(format, args...)
}

// EnsureCustomer implements BillingProvider.
func (p *NoopProvider) EnsureCustomer(_ context.Context, req CustomerRequest) (CustomerHandle, error) {
	if req.TenantID == "" {
		return CustomerHandle{}, errors.New("billing: noop: tenant_id is required")
	}
	id := p.nextID("noop_cus")
	p.logf("billing.noop: ensure_customer tenant=%s ref=%s email=%s", req.TenantID, id, req.Email)
	return CustomerHandle{Provider: p.Name(), ProviderRef: id}, nil
}

// EnsureSubscription implements BillingProvider.
func (p *NoopProvider) EnsureSubscription(_ context.Context, req SubscriptionRequest) (SubscriptionHandle, error) {
	if req.TenantID == "" {
		return SubscriptionHandle{}, errors.New("billing: noop: tenant_id is required")
	}
	id := p.nextID("noop_sub")
	p.logf("billing.noop: ensure_subscription tenant=%s ref=%s plan=%s", req.TenantID, id, req.PlanID)
	return SubscriptionHandle{Provider: p.Name(), ProviderRef: id, Status: "active"}, nil
}

// ReportUsage implements BillingProvider.
func (p *NoopProvider) ReportUsage(_ context.Context, events []UsageEvent) error {
	p.logf("billing.noop: report_usage events=%d", len(events))
	return nil
}

// IssueInvoice implements BillingProvider.
func (p *NoopProvider) IssueInvoice(_ context.Context, req InvoiceRequest) (InvoiceHandle, error) {
	if req.TenantID == "" {
		return InvoiceHandle{}, errors.New("billing: noop: tenant_id is required")
	}
	id := p.nextID("noop_inv")
	p.logf("billing.noop: issue_invoice tenant=%s ref=%s items=%d", req.TenantID, id, len(req.LineItems))
	return InvoiceHandle{Provider: p.Name(), ProviderRef: id}, nil
}

// CancelSubscription implements BillingProvider.
func (p *NoopProvider) CancelSubscription(_ context.Context, subscriptionID string) error {
	if subscriptionID == "" {
		return errors.New("billing: noop: subscription_id is required")
	}
	p.logf("billing.noop: cancel_subscription ref=%s", subscriptionID)
	return nil
}

// static interface check
var _ BillingProvider = (*NoopProvider)(nil)
