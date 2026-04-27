// MetricsBillingSink wraps a billing.BillingSink and bumps
// Prometheus counters on each Emit. The wrapper preserves the
// underlying sink's Emit semantics so the existing audit trail
// continues to work; metrics are a side-effect.
package metrics

import (
	"context"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

// BillingSink is the subset of billing sinks the wrapper accepts.
// It mirrors billing.BillingSink so this package can avoid a hard
// dependency cycle.
type BillingSink interface {
	Emit(event billing.UsageEvent)
}

// MetricsBillingSink is a BillingSink that increments Prometheus
// counters as a side effect of each Emit.
type MetricsBillingSink struct {
	Inner    BillingSink
	Registry *Registry
}

// NewMetricsBillingSink wraps inner so its emissions also bump
// counters on registry.
func NewMetricsBillingSink(inner BillingSink, registry *Registry) *MetricsBillingSink {
	return &MetricsBillingSink{Inner: inner, Registry: registry}
}

// Emit forwards to the wrapped sink and updates counters keyed by
// dimension. Unknown dimensions are forwarded but not counted.
func (s *MetricsBillingSink) Emit(event billing.UsageEvent) {
	if s.Registry != nil {
		switch event.Dimension {
		case billing.CacheHits:
			s.Registry.IncCacheHit()
		case billing.CacheMisses:
			s.Registry.IncCacheMiss()
		case billing.DedupHits:
			s.Registry.IncDedupHit()
		case billing.DedupBytesSaved:
			s.Registry.AddDedupBytesSaved(event.Delta)
		}
	}
	if s.Inner != nil {
		s.Inner.Emit(event)
	}
}

// Close forwards to the inner sink when it implements a
// (context) closer (e.g. *billing.ClickHouseSink). Without this,
// wrapping the billing sink hides the inner Close so the gateway
// shutdown path skips the final flush and drops buffered events.
func (s *MetricsBillingSink) Close(ctx context.Context) error {
	if s == nil || s.Inner == nil {
		return nil
	}
	if c, ok := s.Inner.(interface{ Close(context.Context) error }); ok {
		return c.Close(ctx)
	}
	return nil
}
