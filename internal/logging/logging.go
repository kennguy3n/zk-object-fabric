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
	handler := newHandler(os.Stderr, level, compact)
	logger := slog.New(handler).With("component", component)
	slog.SetDefault(logger)

	// Bridge the legacy `log` package through slog so existing
	// log.Printf("gateway: ...") calls emit the same structured
	// record as a native slog call. LstdFlags is reset to 0 so the
	// std logger doesn't prepend a timestamp the slog handler would
	// duplicate. Bridge records are emitted at INFO regardless of
	// LOG_LEVEL: their producers (this codebase's operational
	// log.Printf calls and third-party libraries) intend them as
	// informational, and letting LOG_LEVEL push them to ERROR would
	// distort error-rate alerting. Using logger.Handler() (not the
	// raw handler) preserves the "component" attribute on bridged
	// records too.
	bridge := slog.NewLogLogger(logger.Handler(), slog.LevelInfo)
	log.SetFlags(0)
	log.SetOutput(bridge.Writer())

	return logger
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

// newHandler picks JSON vs text based on LOG_FORMAT, falling back to
// text under the compact profile and json otherwise when LOG_FORMAT
// is unset. An explicit LOG_FORMAT always wins over the profile
// default.
func newHandler(w io.Writer, level slog.Level, compact bool) slog.Handler {
	opts := &slog.HandlerOptions{Level: level}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_FORMAT"))) {
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
