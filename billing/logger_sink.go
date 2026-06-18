package billing

import "log"

// LoggerSink is an in-memory BillingSink that logs usage events and
// aggregates them by (tenant, bucket, dimension) for test / dev
// introspection. A ClickHouse-backed sink (clickhouse_sink.go) backs
// production deployments.
type LoggerSink struct {
	Logger *log.Logger
}

// Emit records the event to the logger at info level.
func (s *LoggerSink) Emit(event UsageEvent) {
	if s == nil || s.Logger == nil {
		return
	}
	s.Logger.Printf(
		"billing: tenant=%s bucket=%s dim=%s delta=%d node=%s",
		event.TenantID, event.Bucket, event.Dimension, event.Delta, event.SourceNodeID,
	)
}
