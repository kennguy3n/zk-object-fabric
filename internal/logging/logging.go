// Package logging configures the gateway's process-wide structured
// logger and bridges the legacy standard-library `log` package onto
// the same handler.
//
// The gateway historically logged through the standard `log`
// package (log.Printf / log.Fatalf with a "gateway: " prefix). That
// output is unstructured text, which a production log shipper cannot
// index by field. Init wires slog as the process default AND points
// the std `log` package's output at the same slog handler, so every
// existing log.Printf call emits the same structured record as a
// native slog call without rewriting hundreds of call sites.
//
// Configuration
//
// Two environment variables tune the handler at process start,
// mirroring the zk-drive server so a single operator runbook covers
// both services:
//
//   - LOG_LEVEL: debug | info | warn | error (default: info).
//     Compared case-insensitively. Unknown values fall back to info
//     rather than crashing — a log-config typo must never be the
//     thing that stops the gateway from booting.
//   - LOG_FORMAT: json | text (default: json). JSON is the
//     production default because every log shipper indexes it for
//     free; text is for a developer tailing the gateway locally.
//
// Deployment profile
//
// ZKOF_PROFILE=compact selects the single-node SME posture: when set
// and LOG_FORMAT is unset, the format default flips from json to
// text so a small-business operator running the embedded/single-node
// gateway gets human-readable logs on stdout without configuring a
// shipper. An explicit LOG_FORMAT always wins over the profile
// default.
package logging

import (
	"context"
	"io"
	"log"
	"log/slog"
	"os"
	"strings"
)

// CompactProfile is the ZKOF_PROFILE value that selects the
// single-node SME posture (text logs by default).
const CompactProfile = "compact"

// Init configures the process-wide default slog logger and bridges
// the standard `log` package onto the same handler. It returns the
// logger for callers that want to hold a reference. Safe to call
// once at process startup, before any other subsystem logs, so the
// very first config-load error is already structured.
//
// `component` is attached as an attribute on every record so a
// multi-binary deployment can filter by binary without parsing the
// message string.
func Init(component string) *slog.Logger {
	compact := isCompactProfile(os.Getenv("ZKOF_PROFILE"))
	level := parseLevel(os.Getenv("LOG_LEVEL"))
	format := os.Getenv("LOG_FORMAT")
	handler := newHandler(os.Stderr, level, compact, format)
	logger := slog.New(handler).With("component", component)
	slog.SetDefault(logger)

	// Bridge the legacy `log` package through slog so existing
	// log.Printf("gateway: ...") calls emit the same structured
	// record as a native slog call. LstdFlags is reset to 0 so the
	// std logger doesn't prepend a timestamp the slog handler would
	// duplicate.
	//
	// The bridge wraps the handler in unfilteredHandler so bridged
	// records ALWAYS emit regardless of LOG_LEVEL. This is critical:
	// the gateway uses log.Fatalf for ~20+ fatal startup failures
	// (config validation, DB connections, security guards). Without
	// the wrapper, LOG_LEVEL=warn|error makes the handler's
	// Enabled(INFO) return false, so slog.NewLogLogger's writer
	// silently discards every bridged line — the process would exit
	// 1 on a fatal with no diagnostic at all. The std `log` package
	// has no level concept, so every call through it is treated as
	// must-emit. Using logger.Handler() (not the raw handler)
	// preserves the "component" attribute on bridged records.
	bridge := slog.NewLogLogger(unfilteredHandler{logger.Handler()}, slog.LevelInfo)
	log.SetFlags(0)
	log.SetOutput(bridge.Writer())

	return logger
}

// unfilteredHandler wraps a slog.Handler so Enabled always reports
// true, letting records pass the level filter while still being
// formatted (and carrying the embedded attributes) by the wrapped
// handler. Used only for the std-`log` bridge, whose records have no
// level of their own and must never be dropped (see Init).
type unfilteredHandler struct{ slog.Handler }

func (unfilteredHandler) Enabled(context.Context, slog.Level) bool { return true }

// WithAttrs and WithGroup re-wrap the derived handler so the
// always-enabled property survives derivation. Without these overrides
// the embedded slog.Handler would be promoted, returning a plain
// handler that reverts to normal level filtering — silently defeating
// the bridge if this type is ever used somewhere that derives a child
// handler. slog.NewLogLogger's writer never derives today, but keeping
// the wrapper closed under derivation removes the latent trap.
func (u unfilteredHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return unfilteredHandler{u.Handler.WithAttrs(attrs)}
}

func (u unfilteredHandler) WithGroup(name string) slog.Handler {
	return unfilteredHandler{u.Handler.WithGroup(name)}
}

// NewStdLogger returns a *log.Logger that writes through the
// process-wide slog handler instead of straight to a stream, so the
// many subsystems that accept a *log.Logger (read-repair, billing,
// lifecycle, rebalancer, …) emit the same structured records as a
// native slog call rather than unstructured prefixed text on stdout.
// `subsystem` is attached as a structured "subsystem" attribute so a
// shipper can filter by it, replacing the old text prefix.
//
// Unlike the std-`log` bridge installed by Init, records from these
// loggers ARE subject to the LOG_LEVEL filter: they are ordinary
// operational logs (not fatal startup diagnostics) emitted at INFO, so
// LOG_LEVEL=warn|error legitimately quiets them. Must be called after
// Init so slog.Default() is the configured handler; the returned
// logger is always usable.
func NewStdLogger(subsystem string) *log.Logger {
	h := slog.Default().Handler()
	if subsystem != "" {
		h = h.WithAttrs([]slog.Attr{slog.String("subsystem", subsystem)})
	}
	return slog.NewLogLogger(h, slog.LevelInfo)
}

// isCompactProfile reports whether the raw ZKOF_PROFILE value selects
// the compact posture. Case-insensitive after trimming.
func isCompactProfile(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), CompactProfile)
}

// parseLevel maps the user-facing LOG_LEVEL values to slog
// constants. Unknown values fall back to info.
func parseLevel(raw string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	default:
		// Includes "" (unset) and "info".
		return slog.LevelInfo
	}
}

// newHandler picks JSON vs text based on the resolved LOG_FORMAT
// value, falling back to text under the compact profile and json
// otherwise when format is unset. An explicit format always wins over
// the profile default. format is passed in (read once in Init) rather
// than read from the environment here so the function is a pure
// mapping of its inputs — matching how level and compact are resolved
// in Init and keeping it trivially testable.
func newHandler(w io.Writer, level slog.Level, compact bool, format string) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "text":
		return slog.NewTextHandler(w, opts)
	case "json":
		return slog.NewJSONHandler(w, opts)
	default:
		if compact {
			return slog.NewTextHandler(w, opts)
		}
		return slog.NewJSONHandler(w, opts)
	}
}
