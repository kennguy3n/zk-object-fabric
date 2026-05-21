package version

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
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
	// On an unstamped build (no `-ldflags -X …version.Version=…`)
	// the package defaults must keep a recognizable shape so log
	// aggregators that alert on the literal "unknown" string
	// still fire and so consumers parsing Version see a
	// semver-shaped string starting with "0.0.0".
	//
	// On a stamped build (CI passes VERSION via -ldflags) the
	// dev-default invariant by definition does not hold, so the
	// test skips with a message identifying the active stamp
	// rather than logging-and-passing silently — a previous
	// version of this test called t.Logf without t.Errorf and
	// never failed regardless of the package-var state, which
	// made it effectively a no-op. The current shape asserts
	// what it can assert and explicitly skips what it cannot.
	got := Current()
	if got.Version != "0.0.0-dev" {
		t.Skipf("ldflags stamp active (Version=%q GitCommit=%q); dev-default assertions only meaningful on unstamped builds", got.Version, got.GitCommit)
	}
	if !strings.HasPrefix(got.Version, "0.0.0") {
		t.Errorf("Version dev default = %q, want prefix 0.0.0", got.Version)
	}
	if got.GitCommit != "unknown" {
		t.Errorf("GitCommit dev default = %q, want \"unknown\"", got.GitCommit)
	}
	if got.BuildDate != "unknown" {
		t.Errorf("BuildDate dev default = %q, want \"unknown\"", got.BuildDate)
	}
	// runtime triple must still populate on dev builds — it
	// comes from runtime.* at call time and is independent of
	// the ldflags stamp.
	if got.GoVersion == "" || got.GoArch == "" || got.GoOS == "" {
		t.Errorf("runtime triple empty on dev build: %+v", got)
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
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("HEAD Cache-Control = %q", got)
	}
}

// TestHandler_HeadAndGetReturnSameContentLength is the RFC 9110
// §9.3.2 compliance guard: a HEAD response's Content-Length MUST
// equal what a GET response would deliver. Monitoring tooling
// that uses HEAD to compute payload size (AWS ALB target-group
// health checks, ngrok inspector, Prometheus blackbox_exporter's
// probe_http_content_length metric on HEAD) depends on this
// invariant.
//
// Pre-fix the handler used json.NewEncoder(w).Encode directly,
// which writes the body before Content-Length can be set. HEAD
// responses therefore omitted Content-Length entirely, and GET
// responses left it to Go's net/http chunked-transfer fallback.
// Post-fix the handler buffers the body so both GET and HEAD
// carry an identical, deterministic Content-Length.
func TestHandler_HeadAndGetReturnSameContentLength(t *testing.T) {
	getRec := httptest.NewRecorder()
	Handler().ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, Path, nil))
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status=%d, want 200; body=%s", getRec.Code, getRec.Body)
	}
	getCL := getRec.Header().Get("Content-Length")
	if getCL == "" {
		t.Fatal("GET response missing Content-Length header")
	}
	// Sanity: declared length matches actual body length.
	if got, want := getCL, strconv.Itoa(getRec.Body.Len()); got != want {
		t.Errorf("GET Content-Length = %q, want %q (the actual body length)", got, want)
	}

	headRec := httptest.NewRecorder()
	Handler().ServeHTTP(headRec, httptest.NewRequest(http.MethodHead, Path, nil))
	if headRec.Code != http.StatusOK {
		t.Fatalf("HEAD status=%d, want 200", headRec.Code)
	}
	headCL := headRec.Header().Get("Content-Length")
	if headCL == "" {
		t.Fatal("HEAD response missing Content-Length header — RFC 9110 §9.3.2 violation")
	}
	if headCL != getCL {
		t.Errorf("HEAD Content-Length = %q, GET Content-Length = %q; RFC 9110 §9.3.2 requires equality", headCL, getCL)
	}
	if headRec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0; Content-Length header is correct but body must be empty on HEAD", headRec.Body.Len())
	}
}

func TestPath_IsInternalVersion(t *testing.T) {
	if Path != "/internal/version" {
		t.Errorf("Path = %q, want /internal/version", Path)
	}
}
