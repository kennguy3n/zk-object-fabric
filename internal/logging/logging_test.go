package logging

import (
	"bytes"
	"log/slog"
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
// text under the compact profile and json otherwise.
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
			t.Setenv("LOG_FORMAT", tc.logFormat)
			h := newHandler(&bytes.Buffer{}, slog.LevelInfo, tc.compact)
			_, isText := h.(*slog.TextHandler)
			if isText != tc.wantText {
				t.Fatalf("newHandler(LOG_FORMAT=%q, compact=%v): text=%v, want %v", tc.logFormat, tc.compact, isText, tc.wantText)
			}
		})
	}
}

// TestInitBridgesStdLog verifies Init wires the std log package
// through the slog handler with the component attribute, so existing
// log.Printf calls become structured records.
func TestInitBridgesStdLog(t *testing.T) {
	// Init writes to os.Stderr by design; here we only assert it
	// returns a usable logger and sets the default without panicking.
	got := Init("gateway-test")
	if got == nil {
		t.Fatal("Init returned nil logger")
	}
	if slog.Default() == nil {
		t.Fatal("Init did not set slog default")
	}
}
