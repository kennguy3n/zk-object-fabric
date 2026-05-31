// Package audit_test contains drift-detection tests for the
// external audit hand-off bundle (deploy/audit-handoff/).
//
// Three invariants are pinned at every commit:
//
//  1. structural: every component listed in
//     deploy/audit-handoff/manifest.yaml is mentioned by its `id:`
//     (or its `title:`) in deploy/audit-handoff/README.md. The
//     auditor's first-read doc cannot silently drop a component
//     that the bundler still ships.
//
//  2. structural (reverse): every `<id>` slug referenced in
//     deploy/audit-handoff/README.md resolves to a real entry
//     in the manifest. A stale README that references a
//     removed component fails this direction.
//
//  3. filesystem: every PRESENT path under a non-optional
//     component resolves to a real file on disk. Optional
//     components are allowed to have missing paths (the bundler
//     emits placeholders); the test logs them so reviewers
//     can see which prerequisite PRs are still open.
//
// The tests are pure-Go: no shelling out, no subprocess, no
// `make` invocation. They read the manifest and README as files
// and check `os.Stat`. This means they run in the standard
// CI matrix without any extra environment setup.
//
// When a new workstream lands a new component, the canonical
// procedure is:
//
//   1. Add a `components:` entry to manifest.yaml.
//   2. Add a matching section to README.md (referencing the id
//      and/or title).
//   3. Run this test package — failures are descriptive enough
//      that the operator does not need to read the source.
package audit_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// manifest is a local-mirror schema of the bundler's Manifest
// type. We intentionally do NOT import the bundler's exported
// type here because the bundler is a `package main` (binary)
// and a test in a separate module path cannot import it. Mirroring
// the schema is cheap (10 fields) and the structural drift test
// catches mismatches between this local schema and the bundler's
// schema at the YAML-load layer (an unknown field here means the
// bundler grew a feature the dossier test does not know about,
// so the test fails loudly until both are updated).
type manifest struct {
	Version    int         `yaml:"version"`
	BundleName string      `yaml:"bundle_name"`
	OutputDir  string      `yaml:"output_dir"`
	Components []component `yaml:"components"`
}

type component struct {
	ID          string   `yaml:"id"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	PROrigin    string   `yaml:"pr_origin"`
	Paths       []string `yaml:"paths"`
	MakeTarget  string   `yaml:"make_target,omitempty"`
	Optional    bool     `yaml:"optional"`
}

const (
	manifestRel = "deploy/audit-handoff/manifest.yaml"
	readmeRel   = "deploy/audit-handoff/README.md"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	// The test is in tests/audit/; the repo root is two levels up.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %q does not contain go.mod: %v (test must run from tests/audit/)", root, err)
	}
	return root
}

func loadManifest(t *testing.T) (string, *manifest) {
	t.Helper()
	root := repoRoot(t)
	mp := filepath.Join(root, manifestRel)
	raw, err := os.ReadFile(mp)
	if err != nil {
		t.Fatalf("read manifest %s: %v", manifestRel, err)
	}
	var m manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("parse manifest %s: %v (if the bundler grew a new field, mirror it in this test's `component` struct too)", manifestRel, err)
	}
	return root, &m
}

func loadReadme(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	rp := filepath.Join(root, readmeRel)
	raw, err := os.ReadFile(rp)
	if err != nil {
		t.Fatalf("read readme %s: %v", readmeRel, err)
	}
	return string(raw)
}

// TestHandoffManifest_WellFormed parses the manifest with KnownFields=true
// (typo'd field rejection) and checks the trivial well-formedness
// invariants. This is intentionally a fast precondition for the
// other tests in this package: if the manifest is malformed, every
// later test would fail with confusing messages.
func TestHandoffManifest_WellFormed(t *testing.T) {
	_, m := loadManifest(t)
	if m.Version != 1 {
		t.Fatalf("manifest version = %d, want 1 (this test pins the schema version it understands; if you bump the bundler's schema, update this test too)", m.Version)
	}
	if m.BundleName == "" {
		t.Errorf("bundle_name is empty")
	}
	if len(m.Components) == 0 {
		t.Fatalf("manifest has zero components")
	}
	seen := make(map[string]bool, len(m.Components))
	for i, c := range m.Components {
		if c.ID == "" {
			t.Errorf("components[%d]: id is empty", i)
		}
		if c.Title == "" {
			t.Errorf("components[%d] (%s): title is empty", i, c.ID)
		}
		if len(c.Paths) == 0 {
			t.Errorf("components[%d] (%s): no paths declared", i, c.ID)
		}
		if c.PROrigin == "" {
			t.Errorf("components[%d] (%s): pr_origin is empty — every component must cite its originating PR for auditor traceability", i, c.ID)
		}
		if seen[c.ID] {
			t.Errorf("components[%d] (%s): duplicate id", i, c.ID)
		}
		seen[c.ID] = true
	}
	hasRequired := false
	for _, c := range m.Components {
		if !c.Optional {
			hasRequired = true
			break
		}
	}
	if !hasRequired {
		t.Errorf("manifest has no non-optional component — the bundle has no anchored content. At least one component (today: progress_pin) must be optional=false so a fresh-cut branch still produces a meaningful bundle.")
	}
}

// TestHandoffReadme_MentionsEveryComponent pins the structural
// invariant that the auditor's first-read doc must mention every
// component the bundler ships. Catches "I added a component to
// the YAML but forgot to write the auditor onboarding for it."
//
// Match policy: we look for either the component `id` (case-
// insensitive, surrounded by word-ish boundaries) OR the
// component `title` (also case-insensitive). Either is fine —
// the README may refer to "S3 protocol conformance matrix" in
// prose without using the literal slug `conformance_matrix`.
func TestHandoffReadme_MentionsEveryComponent(t *testing.T) {
	_, m := loadManifest(t)
	readme := loadReadme(t)
	lower := strings.ToLower(readme)
	var missing []string
	for _, c := range m.Components {
		idHit := strings.Contains(lower, strings.ToLower(c.ID))
		titleHit := strings.Contains(lower, strings.ToLower(c.Title))
		if !idHit && !titleHit {
			missing = append(missing, fmt.Sprintf("%s (%q)", c.ID, c.Title))
		}
	}
	if len(missing) > 0 {
		t.Errorf("deploy/audit-handoff/README.md does not mention these manifest components:\n  - %s\n\nEither the component is intentional and the README needs a new section, or the component was renamed and the README's old reference is stale.",
			strings.Join(missing, "\n  - "))
	}
}

// TestHandoffReadme_NoStaleComponentReferences is the reverse
// direction of TestHandoffReadme_MentionsEveryComponent. Catches
// "I renamed `dr_runbook` to `dr_runbooks` in the YAML but the
// README still says `dr_runbook`."
//
// Detection policy: scan for snake_case tokens that are NOT in
// path/filename context. A token in path/filename context is one
// whose preceding character is `/` or `_` or `-` (mid-path) OR
// whose following character is `.` (file extension), `/` (path
// continuation), or `-` (hyphenated continuation). This excludes
// substrings like `collect_evidence` inside `collect_evidence.sh`
// and `tests_dr` inside `tests_dr_verifier.go.MISSING` while
// still catching bare prose references such as "the dr_runbook
// component" or backtick-quoted `dr_runbook`.
//
// The regex is intentionally permissive on what it MATCHES (any
// snake_case identifier) and the boundary check does the work of
// excluding false positives. This keeps the regex readable and
// the rejection logic auditable.
func TestHandoffReadme_NoStaleComponentReferences(t *testing.T) {
	_, m := loadManifest(t)
	readme := loadReadme(t)

	known := make(map[string]bool, len(m.Components))
	for _, c := range m.Components {
		known[c.ID] = true
	}

	idRE := regexp.MustCompile(`[a-z][a-z0-9]*_[a-z0-9_]+`)
	indices := idRE.FindAllStringIndex(readme, -1)
	seen := make(map[string]bool, len(indices))
	var unknown []string
	for _, idx := range indices {
		start, end := idx[0], idx[1]
		tok := readme[start:end]
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if isPathContext(readme, start, end) {
			continue
		}
		if known[tok] {
			continue
		}
		unknown = append(unknown, tok)
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		t.Errorf("deploy/audit-handoff/README.md references unknown component ids:\n  - %s\n\nManifest declares: %s.\nEither the README is stale (a component was renamed) or the snake_case token is something other than a component id and isPathContext() did not catch it — in the latter case extend the boundary check in this test.",
			strings.Join(unknown, "\n  - "),
			strings.Join(manifestIDs(m), ", "))
	}
}

// isPathContext returns true when readme[start:end] is a
// snake_case fragment embedded in a filename or path token.
// Returns true if the character immediately before `start` is
// `/`, `_`, or `-` (mid-path/mid-identifier) OR the character
// immediately at `end` is `.`, `/`, or `-` (extension /
// path-continuation / hyphenated-continuation). Edge case:
// if start==0 or end==len(readme) the corresponding boundary
// is treated as a non-path character (top/bottom of file).
func isPathContext(s string, start, end int) bool {
	if start > 0 {
		switch s[start-1] {
		case '/', '_', '-':
			return true
		}
	}
	if end < len(s) {
		switch s[end] {
		case '.', '/', '-':
			return true
		}
	}
	return false
}

func manifestIDs(m *manifest) []string {
	out := make([]string, 0, len(m.Components))
	for _, c := range m.Components {
		out = append(out, c.ID)
	}
	sort.Strings(out)
	return out
}

// TestHandoffManifest_PathsResolveOrAreOptional asserts that
// every PATH under a non-optional component exists on disk. For
// OPTIONAL components, missing paths are logged (so the operator
// running the CI can see which prerequisite PRs are still open)
// but do not fail the test.
//
// This is the test that flips from green to red on `main` once
// you decide a once-optional component should be load-bearing:
// remove `optional: true` from manifest.yaml and any missing
// path becomes a hard failure here.
func TestHandoffManifest_PathsResolveOrAreOptional(t *testing.T) {
	root, m := loadManifest(t)
	var hard []string
	var soft []string
	for _, c := range m.Components {
		for _, p := range c.Paths {
			abs := filepath.Join(root, p)
			if _, err := os.Stat(abs); err != nil {
				entry := fmt.Sprintf("%s :: %s (origin %s)", c.ID, p, c.PROrigin)
				if c.Optional {
					soft = append(soft, entry)
					continue
				}
				hard = append(hard, entry)
			}
		}
	}
	for _, s := range soft {
		// t.Logf is visible in `go test -v` output and in CI
		// logs; it does not fail the test. Reviewers reading
		// the CI log see exactly which prereq PRs are still
		// open without having to cross-walk PR numbers.
		t.Logf("audit-handoff: optional path not on this branch yet: %s", s)
	}
	if len(hard) > 0 {
		t.Errorf("deploy/audit-handoff/manifest.yaml lists REQUIRED paths that do not exist on disk:\n  - %s\n\nEither the path was renamed (update the manifest), or this is a once-optional component that has just been promoted to required and the source PR has not landed yet (revert the optional: false change on this branch).",
			strings.Join(hard, "\n  - "))
	}
}

// TestHandoffManifest_NoDuplicatePaths catches the "I added a
// path under two different components by mistake" failure. The
// bundler will technically write the file twice (under each
// component's directory) which is harmless but wastes auditor
// time. Pinning here prevents the drift.
func TestHandoffManifest_NoDuplicatePaths(t *testing.T) {
	_, m := loadManifest(t)
	owner := make(map[string]string, 32)
	var dups []string
	for _, c := range m.Components {
		for _, p := range c.Paths {
			if prev, ok := owner[p]; ok {
				dups = append(dups, fmt.Sprintf("%q claimed by both %s and %s", p, prev, c.ID))
				continue
			}
			owner[p] = c.ID
		}
	}
	if len(dups) > 0 {
		t.Errorf("manifest has duplicate path ownership:\n  - %s", strings.Join(dups, "\n  - "))
	}
}
