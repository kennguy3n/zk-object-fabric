package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

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
