package logging

import (
	"bytes"
	"log"
	"log/slog"
	"strings"
	"testing"
)

// TestParseLevel pins the LOG_LEVEL parser: documented values and
// aliases map to the expected slog.Level, unknown values fall back
// to info rather than erroring.
func TestParseLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"DEBUG", slog.LevelDebug},
		{" debug ", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"err", slog.LevelError},
		{"verbose", slog.LevelInfo}, // unknown → info
	}
	for _, tc := range cases {
		if got := parseLevel(tc.in); got != tc.want {
			t.Fatalf("parseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestIsCompactProfile pins the ZKOF_PROFILE=compact matcher.
func TestIsCompactProfile(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"compact", true},
		{"Compact", true},
		{"  COMPACT ", true},
		{"", false},
		{"single-node", false},
	}
	for _, tc := range cases {
		if got := isCompactProfile(tc.in); got != tc.want {
			t.Fatalf("isCompactProfile(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestNewHandlerFormatSelection pins the LOG_FORMAT / compact-profile
// interaction: explicit LOG_FORMAT always wins; unset defaults to
// text under the compact profile and json otherwise. format is now a
// parameter, so the test passes it directly instead of via t.Setenv.
func TestNewHandlerFormatSelection(t *testing.T) {
	cases := []struct {
		name      string
		logFormat string
		compact   bool
		wantText  bool
	}{
		{"unset/full→json", "", false, false},
		{"unset/compact→text", "", true, true},
		{"explicit text wins", "text", false, true},
		{"explicit json wins over compact", "json", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHandler(&bytes.Buffer{}, slog.LevelInfo, tc.compact, tc.logFormat)
			_, isText := h.(*slog.TextHandler)
			if isText != tc.wantText {
				t.Fatalf("newHandler(LOG_FORMAT=%q, compact=%v): text=%v, want %v", tc.logFormat, tc.compact, isText, tc.wantText)
			}
		})
	}
}

// restoreLogGlobals snapshots the process-wide slog default and std
// log output/flags so a test that calls Init can restore them on
// cleanup, keeping global-state mutation from leaking into sibling
// tests.
func restoreLogGlobals(t *testing.T) {
	t.Helper()
	prevDefault := slog.Default()
	prevFlags := log.Flags()
	prevOut := log.Writer()
	t.Cleanup(func() {
		slog.SetDefault(prevDefault)
		log.SetFlags(prevFlags)
		log.SetOutput(prevOut)
	})
}

// TestInitBridgesStdLog verifies Init wires the std log package
// through the slog handler with the component attribute, so existing
// log.Printf calls become structured records.
func TestInitBridgesStdLog(t *testing.T) {
	restoreLogGlobals(t)
	got := Init("gateway-test")
	if got == nil {
		t.Fatal("Init returned nil logger")
	}
	if slog.Default() == nil {
		t.Fatal("Init did not set slog default")
	}
}

// TestBridgeEmitsAboveLogLevel is the regression test for the bug
// where the std-log bridge silently dropped log.Printf / log.Fatalf
// output whenever LOG_LEVEL resolved above info. The bridge must emit
// regardless of LOG_LEVEL so fatal startup diagnostics are never
// swallowed. Asserts on the wiring used by Init (unfilteredHandler)
// against an error-level handler.
func TestBridgeEmitsAboveLogLevel(t *testing.T) {
	restoreLogGlobals(t)
	var buf bytes.Buffer
	// Handler floor at error — a native Info record would be dropped.
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	logger := slog.New(handler).With("component", "gateway")
	bridge := slog.NewLogLogger(unfilteredHandler{logger.Handler()}, slog.LevelInfo)
	bridge.Printf("gateway: fatal config error")
	if !strings.Contains(buf.String(), "fatal config error") {
		t.Fatalf("bridged log output dropped at LOG_LEVEL=error; got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "\"component\":\"gateway\"") {
		t.Fatalf("bridged record missing component attribute; got %q", buf.String())
	}
}

// TestNewStdLoggerRespectsLevel verifies subsystem loggers created by
// NewStdLogger DO honour the LOG_LEVEL filter (unlike the must-emit
// bridge) and carry the subsystem attribute.
func TestNewStdLoggerRespectsLevel(t *testing.T) {
	restoreLogGlobals(t)
	var buf bytes.Buffer
	handler := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	slog.SetDefault(slog.New(handler).With("component", "gateway"))

	lg := NewStdLogger("billing")
	lg.Printf("charged tenant")
	if buf.Len() != 0 {
		t.Fatalf("NewStdLogger emitted an info line at LOG_LEVEL=error; got %q", buf.String())
	}

	buf.Reset()
	handler2 := slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(handler2).With("component", "gateway"))
	lg = NewStdLogger("billing")
	lg.Printf("charged tenant")
	if !strings.Contains(buf.String(), "\"subsystem\":\"billing\"") {
		t.Fatalf("NewStdLogger record missing subsystem attribute; got %q", buf.String())
	}
}
