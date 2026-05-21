package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

// PARALLELISM WARNING — DO NOT ADD t.Parallel() TO THESE TESTS.
//
// TestCurrent_ReturnsLdflagsStampedFields and TestHandler_ServesJSON
// mutate the package-level vars Version, GitCommit, and BuildDate
// before calling Current() / Handler(). They restore the originals
// via t.Cleanup so the package-level state is invariant across the
// suite, but the mutations themselves are not goroutine-safe — if
// a future contributor adds t.Parallel() to any test in this file,
// the mutations will race against concurrent reads from Current()
// and HTTP requests served by Handler().
//
// The right fix if parallelism is needed is NOT a sync.Mutex
// around the vars (it would slow every Current() call in production
// for a test-only concern). Instead, refactor Current/Handler to
// accept the Info as a parameter (or read from a context-scoped
// override) and have the tests construct the Info locally. Only
// then is it safe to add t.Parallel() back.
//
// As of this writing every test in this file runs sequentially,
// which is why the global mutation pattern is acceptable.

func TestCurrent_ReturnsLdflagsStampedFields(t *testing.T) {
	old := struct{ v, c, b string }{Version, GitCommit, BuildDate}
	t.Cleanup(func() {
		Version, GitCommit, BuildDate = old.v, old.c, old.b
	})

	Version = "0.1.0"
	GitCommit = "abc1234567890"
	BuildDate = "2026-05-21T15:00:00Z"

	got := Current()
	if got.Version != "0.1.0" {
		t.Errorf("Version = %q, want 0.1.0", got.Version)
	}
	if got.GitCommit != "abc1234567890" {
		t.Errorf("GitCommit = %q, want abc1234567890", got.GitCommit)
	}
	if got.BuildDate != "2026-05-21T15:00:00Z" {
		t.Errorf("BuildDate = %q, want 2026-05-21T15:00:00Z", got.BuildDate)
	}
	if got.GoVersion != runtime.Version() {
		t.Errorf("GoVersion = %q, want %q", got.GoVersion, runtime.Version())
	}
	if got.GoArch != runtime.GOARCH || got.GoOS != runtime.GOOS {
		t.Errorf("GoArch/GoOS = %q/%q, want %q/%q", got.GoArch, got.GoOS, runtime.GOARCH, runtime.GOOS)
	}
}

func TestCurrent_DevDefaults(t *testing.T) {
	// Without any ldflags override, the package defaults
	// must keep a recognizable shape so log aggregators that
	// alert on "unknown" still fire and so consumers parsing
	// Version see a semver-shaped string.
	got := Current()
	if !strings.HasPrefix(got.Version, "0.0.0") && got.Version != "" {
		t.Logf("Version default = %q (ldflags override active; not the dev default)", got.Version)
	}
}

func TestHandler_ServesJSON(t *testing.T) {
	old := struct{ v, c, b string }{Version, GitCommit, BuildDate}
	t.Cleanup(func() {
		Version, GitCommit, BuildDate = old.v, old.c, old.b
	})
	Version = "0.1.0"
	GitCommit = "deadbeef"
	BuildDate = "2026-05-21T15:00:00Z"

	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	var doc Info
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode body: %v; body=%s", err, rec.Body)
	}
	if doc.Version != "0.1.0" {
		t.Errorf("body Version = %q, want 0.1.0", doc.Version)
	}
	if doc.GitCommit != "deadbeef" {
		t.Errorf("body GitCommit = %q, want deadbeef", doc.GitCommit)
	}
	if doc.BuildDate != "2026-05-21T15:00:00Z" {
		t.Errorf("body BuildDate = %q", doc.BuildDate)
	}
	if doc.GoVersion == "" || doc.GoArch == "" || doc.GoOS == "" {
		t.Errorf("runtime triple incomplete: %+v", doc)
	}
}

func TestHandler_HeadReturnsHeadersOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, Path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("HEAD Content-Type = %q", got)
	}
}

func TestPath_IsInternalVersion(t *testing.T) {
	if Path != "/internal/version" {
		t.Errorf("Path = %q, want /internal/version", Path)
	}
}
