package main

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newSyntheticRepo writes a small fake repo into t.TempDir() with
// just enough structure to exercise the bundler:
//
//   - a go.mod (so the --repo-root check in main.run() would pass,
//     although we exercise Build directly here)
//   - a manifest.yaml with two components: one required, one
//     optional with one present path and one missing path
//   - the on-disk files referenced by the present paths
//
// The bundler's Build() is then called against this repo and the
// produced tarball is unpacked + inspected. This is the only test
// suite for Build(); it covers happy path, missing-optional path,
// MANIFEST.txt determinism, and INDEX.md correctness.
func newSyntheticRepo(t *testing.T) (repoRoot, manifestPath string) {
	t.Helper()
	repoRoot = t.TempDir()
	mustWrite(t, filepath.Join(repoRoot, "go.mod"), "module example.com/synth\n\ngo 1.25\n")
	mustWrite(t, filepath.Join(repoRoot, "docs", "OVERVIEW.md"), "# overview\n")
	mustWrite(t, filepath.Join(repoRoot, "docs", "GUIDE.md"), "# guide\n")
	mustWrite(t, filepath.Join(repoRoot, "tests", "core", "x.go"), "package core\n")
	// note: tests/optional/missing.go is intentionally NOT written
	manifestPath = filepath.Join(repoRoot, "manifest.yaml")
	mustWrite(t, manifestPath, `
version: 1
bundle_name: synth-bundle
output_dir: build/out
components:
  - id: core
    title: Core
    description: required component
    pr_origin: synthetic
    paths:
      - docs/OVERVIEW.md
      - docs/GUIDE.md
      - tests/core
    optional: false
  # Intentional duplicate of docs/GUIDE.md (also declared by
  # the core component above): the synthetic-only duplication
  # lets these tests exercise Build's hadReal/included/missing
  # accounting -- optional_partial has one present path and one
  # absent path, and we want to confirm it lands in
  # ComponentsIncluded (not ComponentsMissing) because the
  # present path resolves. The real manifest is constrained by
  # the TestHandoffManifest_NoDuplicatePaths drift test in
  # tests/audit/handoff_test.go, which runs only against
  # deploy/audit-handoff/manifest.yaml and never sees this.
  - id: optional_partial
    title: Optional Partial
    description: optional with one present, one missing
    pr_origin: "#999"
    paths:
      - docs/GUIDE.md
      - tests/optional/missing.go
    optional: true
`)
	return
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBuild_HappyPath_MissingOptionalProducesPlaceholder(t *testing.T) {
	repoRoot, manifestPath := newSyntheticRepo(t)
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	res, err := Build(m, BundleOptions{
		RepoRoot:     repoRoot,
		ManifestPath: manifestPath,
		CommitSHA:    "deadbeefcafe",
		BuildTime:    time.Date(2026, 5, 30, 23, 30, 0, 0, time.UTC),
		SkipMake:     true,
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.OutputPath == "" || !strings.HasSuffix(res.OutputPath, ".tar.gz") {
		t.Fatalf("OutputPath = %q, want non-empty .tar.gz", res.OutputPath)
	}
	if !contains(res.ComponentsIncluded, "core") {
		t.Errorf("expected `core` in ComponentsIncluded, got %v", res.ComponentsIncluded)
	}
	if !contains(res.ComponentsIncluded, "optional_partial") {
		// optional_partial has docs/GUIDE.md present, so it
		// IS included (one real file) — the missing path is a
		// placeholder.
		t.Errorf("expected optional_partial in ComponentsIncluded (it has one real path); got %v", res.ComponentsIncluded)
	}
	if len(res.ComponentsMissing) != 0 {
		t.Errorf("expected ComponentsMissing to be empty (optional_partial had at least one real file); got %v", res.ComponentsMissing)
	}

	// Inspect the tarball contents.
	files := readTarball(t, res.OutputPath)
	wantPaths := []string{
		"INDEX.md",
		"MANIFEST.txt",
		"manifest.yaml",
		"core/docs/OVERVIEW.md",
		"core/docs/GUIDE.md",
		"core/tests/core/x.go",
		"optional_partial/docs/GUIDE.md",
		"optional_partial/__MISSING__/tests_optional_missing.go.MISSING",
	}
	for _, p := range wantPaths {
		if _, ok := files[p]; !ok {
			t.Errorf("expected bundle entry %q not found; got keys=%v", p, keys(files))
		}
	}

	// MANIFEST.txt header lines must start with `#` so
	// `sha256sum -c` ignores them. Data lines must use the
	// `<hex>  <path>` format.
	manifestTxt := files["MANIFEST.txt"]
	for i, line := range strings.Split(strings.TrimRight(manifestTxt, "\n"), "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue // header line, ok
		}
		fields := strings.SplitN(line, "  ", 2)
		if len(fields) != 2 || len(fields[0]) != 64 {
			t.Errorf("MANIFEST.txt line %d malformed: %q", i, line)
		}
	}

	// INDEX.md must list both components with their status.
	idx := files["INDEX.md"]
	if !strings.Contains(idx, "core — Core") {
		t.Errorf("INDEX.md missing core component header; got:\n%s", idx)
	}
	if !strings.Contains(idx, "optional_partial — Optional Partial") {
		t.Errorf("INDEX.md missing optional_partial component header; got:\n%s", idx)
	}
	if !strings.Contains(idx, "Commit:  `deadbeefcafe`") {
		t.Errorf("INDEX.md missing commit anchor")
	}

	// Deterministic build: re-run with same inputs, expect same
	// MANIFEST.txt body byte-for-byte.
	res2, err := Build(m, BundleOptions{
		RepoRoot:     repoRoot,
		ManifestPath: manifestPath,
		CommitSHA:    "deadbeefcafe",
		BuildTime:    time.Date(2026, 5, 30, 23, 30, 0, 0, time.UTC),
		SkipMake:     true,
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Build (rerun): %v", err)
	}
	files2 := readTarball(t, res2.OutputPath)
	if files["MANIFEST.txt"] != files2["MANIFEST.txt"] {
		t.Errorf("MANIFEST.txt is not byte-deterministic across runs:\nfirst:\n%s\nsecond:\n%s", files["MANIFEST.txt"], files2["MANIFEST.txt"])
	}
}

func TestBuild_MissingRequiredPathIsHardError(t *testing.T) {
	repoRoot := t.TempDir()
	mustWrite(t, filepath.Join(repoRoot, "go.mod"), "module x\ngo 1.25\n")
	manifestPath := filepath.Join(repoRoot, "manifest.yaml")
	mustWrite(t, manifestPath, `
version: 1
bundle_name: synth
output_dir: out
components:
  - id: required
    title: Required
    description: x
    pr_origin: x
    paths:
      - does/not/exist.md
    optional: false
`)
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	_, err = Build(m, BundleOptions{
		RepoRoot:     repoRoot,
		ManifestPath: manifestPath,
		CommitSHA:    "abc",
		SkipMake:     true,
		Out:          io.Discard,
	})
	if err == nil {
		t.Fatalf("expected error for missing REQUIRED path, got nil")
	}
	if !strings.Contains(err.Error(), "required path") {
		t.Errorf("expected error to mention 'required path'; got: %v", err)
	}
	// Regression for the deferred-cleanup pair (tmpFile close +
	// tmpPath remove) added to Build's prelude. Before the fix
	// the .tmp file was leaked under the OutputDir on every error
	// return, and the file descriptor was leaked along with it.
	// We can't observe the fd leak from a Go test directly, but
	// the on-disk leak is a stable proxy: if the deferred remove
	// fires, the OutputDir contains no .tmp file.
	outDir := filepath.Join(repoRoot, "out")
	entries, readErr := os.ReadDir(outDir)
	if readErr != nil {
		// outDir may not exist if the error fired before MkdirAll;
		// that's also a valid "no leaked .tmp" state.
		return
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leaked tmp file after error: %s/%s", outDir, e.Name())
		}
	}
}

func TestBuild_AllowMissingOptional_OmitsPlaceholder(t *testing.T) {
	repoRoot := t.TempDir()
	mustWrite(t, filepath.Join(repoRoot, "go.mod"), "module x\ngo 1.25\n")
	mustWrite(t, filepath.Join(repoRoot, "real.md"), "hi\n")
	manifestPath := filepath.Join(repoRoot, "manifest.yaml")
	mustWrite(t, manifestPath, `
version: 1
bundle_name: synth
output_dir: out
components:
  - id: anchor
    title: Anchor
    description: x
    pr_origin: x
    paths:
      - real.md
    optional: false
  - id: opt
    title: Opt
    description: x
    pr_origin: "#999"
    paths:
      - not/there.md
    optional: true
`)
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	res, err := Build(m, BundleOptions{
		RepoRoot:             repoRoot,
		ManifestPath:         manifestPath,
		CommitSHA:            "abc",
		SkipMake:             true,
		AllowMissingOptional: true,
		Out:                  io.Discard,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !contains(res.ComponentsMissing, "opt") {
		t.Errorf("expected `opt` in ComponentsMissing (AllowMissingOptional=true means no placeholder, so component contributes nothing); got %v", res.ComponentsMissing)
	}
	files := readTarball(t, res.OutputPath)
	for k := range files {
		if strings.Contains(k, "__MISSING__") {
			t.Errorf("AllowMissingOptional=true but bundle still contains placeholder %q", k)
		}
	}
}

// TestBuild_EmptyOptionalDirNotCountedAsIncluded pins the
// fix for the ANALYSIS_0004 finding: a component whose declared
// path resolves to a directory that exists but contains zero
// regular files (symlinks/sockets only, or genuinely empty) must
// be reported as ComponentsMissing, not ComponentsIncluded.
// Before the fix, `hadReal = true` was set whenever os.Stat
// succeeded, so the component falsely appeared in INDEX.md as
// "included" despite contributing nothing to the tar stream.
func TestBuild_EmptyOptionalDirNotCountedAsIncluded(t *testing.T) {
	repoRoot := t.TempDir()
	mustWrite(t, filepath.Join(repoRoot, "go.mod"), "module x\ngo 1.25\n")
	mustWrite(t, filepath.Join(repoRoot, "anchor.md"), "anchor\n")

	// Make an empty directory that the manifest's optional
	// component will point at. The directory exists (stat
	// succeeds) but writePath's filepath.Walk produces zero
	// regular-file entries.
	if err := os.MkdirAll(filepath.Join(repoRoot, "empty_optional_dir"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	manifestPath := filepath.Join(repoRoot, "manifest.yaml")
	mustWrite(t, manifestPath, `
version: 1
bundle_name: synth
output_dir: out
components:
  - id: anchor
    title: Anchor
    description: x
    pr_origin: x
    paths:
      - anchor.md
    optional: false
  - id: empty_opt
    title: Empty Optional
    description: x
    pr_origin: "#999"
    paths:
      - empty_optional_dir
    optional: true
`)
	m, err := LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	res, err := Build(m, BundleOptions{
		RepoRoot:     repoRoot,
		ManifestPath: manifestPath,
		CommitSHA:    "abc",
		SkipMake:     true,
		Out:          io.Discard,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if contains(res.ComponentsIncluded, "empty_opt") {
		t.Errorf("empty optional dir was wrongly counted as included; res.ComponentsIncluded=%v, res.ComponentsMissing=%v", res.ComponentsIncluded, res.ComponentsMissing)
	}
	if !contains(res.ComponentsMissing, "empty_opt") {
		t.Errorf("empty optional dir should be in ComponentsMissing; got included=%v missing=%v", res.ComponentsIncluded, res.ComponentsMissing)
	}
	// INDEX.md must label this component as MISSING (optional),
	// not "included", so an auditor isn't told to look for files
	// that aren't there.
	files := readTarball(t, res.OutputPath)
	idx := files["INDEX.md"]
	// The component header line is "## empty_opt — Empty Optional";
	// the next status line is "Status: **MISSING (optional)**".
	if !strings.Contains(idx, "empty_opt — Empty Optional") {
		t.Errorf("INDEX.md missing component header; got:\n%s", idx)
	}
	// Find the status line for empty_opt: it must be MISSING,
	// not "included".
	if strings.Contains(idx, "## empty_opt — Empty Optional\n\nStatus: **included**") {
		t.Errorf("INDEX.md falsely reports empty optional dir as included:\n%s", idx)
	}
}

func TestLoadManifest_RejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: out
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: [x]
    optional: false
    bogus_field: 42
`)
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error for unknown field 'bogus_field', got nil")
	}
	if !strings.Contains(err.Error(), "bogus_field") {
		t.Errorf("error should name the unknown field; got: %v", err)
	}
}

func TestLoadManifest_RejectsDuplicateIDs(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: out
components:
  - id: dup
    title: One
    description: x
    pr_origin: x
    paths: [a]
    optional: false
  - id: dup
    title: Two
    description: x
    pr_origin: x
    paths: [b]
    optional: false
`)
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error for duplicate id, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Errorf("error should mention duplicate id; got: %v", err)
	}
}

func TestLoadManifest_RejectsAllOptional(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: out
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: [a]
    optional: true
`)
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error when every component is optional, got nil")
	}
	if !strings.Contains(err.Error(), "non-optional") {
		t.Errorf("error should mention non-optional anchor requirement; got: %v", err)
	}
}

func TestLoadManifest_RejectsInvalidID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: out
components:
  - id: 1bad
    title: X
    description: x
    pr_origin: x
    paths: [a]
    optional: false
`)
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error for id starting with digit, got nil")
	}
}

func TestLoadManifest_RejectsParentTraversalOutputDir(t *testing.T) {
	// Defence-in-depth regression: parallel to the component-paths
	// check below. The bundler does filepath.Join(RepoRoot, OutputDir)
	// + os.MkdirAll, so an "../../tmp"-style output_dir would write
	// the tarball (and create directories) outside the repo root.
	cases := []struct {
		name      string
		outputDir string
	}{
		{"raw_parent", ".."},
		{"parent_prefix", "../tmp"},
		{"deep_parent", "../../../tmp"},
		{"embedded_parent", "build/../../tmp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "m.yaml")
			mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: "`+c.outputDir+`"
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: [a]
    optional: false
`)
			_, err := LoadManifest(p)
			if err == nil {
				t.Fatalf("expected error for output_dir %q escaping repo root, got nil", c.outputDir)
			}
			if !strings.Contains(err.Error(), "output_dir") || !strings.Contains(err.Error(), "escapes the repo root") {
				t.Errorf("error should mention output_dir + repo-root escape; got: %v", err)
			}
		})
	}
}

func TestLoadManifest_RejectsParentTraversal(t *testing.T) {
	// Defence-in-depth regression: the manifest is repo-controlled
	// today, but the bundler resolves component paths with
	// filepath.Join against the repo root. A `..` segment would
	// escape the repo root and let the bundler copy files outside
	// the source tree into the auditor's tarball. validate() must
	// reject the four canonical spellings before Build() ever runs.
	cases := []struct {
		name string
		path string
	}{
		{"raw_parent", ".."},
		{"parent_prefix", "../etc/passwd"},
		{"deep_parent", "../../../etc/passwd"},
		{"embedded_parent", "tests/../../../etc/passwd"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "m.yaml")
			mustWrite(t, p, `
version: 1
bundle_name: x
output_dir: out
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: ["`+c.path+`"]
    optional: false
`)
			_, err := LoadManifest(p)
			if err == nil {
				t.Fatalf("expected error for path %q escaping repo root, got nil", c.path)
			}
			if !strings.Contains(err.Error(), "escapes the repo root") {
				t.Errorf("error should mention repo-root escape; got: %v", err)
			}
		})
	}
}

func TestLoadManifest_RejectsBundleNameWithPathSeparators(t *testing.T) {
	// Defence-in-depth regression: bundle_name is interpolated into
	// the output filename (filepath.Join(outDir, bundleStem+".tar.gz"))
	// and into the in-tar entry prefix (filepath.Join(w.bundleDir, …)).
	// A value containing a path separator or a ".." segment would
	// escape the output directory on disk and produce zip-slip tar
	// entries on extraction. validate() must reject both spellings
	// at load time so Build() never sees a path-bearing bundle_name.
	// YAML quoting note: double-quoted scalars process backslash
	// escapes (e.g. \e -> ESC), so the backslash variant must use
	// single-quoted YAML scalars to preserve the literal backslash.
	cases := []struct {
		name       string
		bundleName string
		wantMsg    string
	}{
		{"forward_slash", "../evil", "path separators"},
		{"backslash", `..\evil`, "path separators"},
		{"slash_only", "a/b", "path separators"},
		{"parent_only", "..", "valid filename stem"},
		{"dot_only", ".", "valid filename stem"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "m.yaml")
			mustWrite(t, p, `
version: 1
bundle_name: '`+c.bundleName+`'
output_dir: out
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: [a]
    optional: false
`)
			_, err := LoadManifest(p)
			if err == nil {
				t.Fatalf("expected error for bundle_name %q, got nil", c.bundleName)
			}
			if !strings.Contains(err.Error(), "bundle_name") || !strings.Contains(err.Error(), c.wantMsg) {
				t.Errorf("error should mention bundle_name + %q; got: %v", c.wantMsg, err)
			}
		})
	}
}

func TestBuild_RequiredComponentEmptyDirHardFails(t *testing.T) {
	// Defence-in-depth regression: a required component whose
	// declared path resolves to an empty / symlink-only directory
	// (stat succeeds, but filepath.Walk yields zero regular files)
	// must hard-fail Build(). Without this guard, the component
	// would be silently absent from the tarball AND mislabeled
	// "MISSING (optional)" in INDEX.md, both of which are wrong
	// for a required component.
	repoRoot := t.TempDir()
	mustWrite(t, filepath.Join(repoRoot, "anchor.md"), "anchor\n")

	// Create an empty directory that the required component will
	// point at. The directory exists (stat succeeds) but contains
	// zero regular files.
	if err := os.MkdirAll(filepath.Join(repoRoot, "empty_required_dir"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}

	mp := filepath.Join(repoRoot, "m.yaml")
	mustWrite(t, mp, `
version: 1
bundle_name: synth
output_dir: out
components:
  - id: anchor
    title: Anchor
    description: x
    pr_origin: "#1"
    paths:
      - anchor.md
    optional: false
  - id: empty_req
    title: Empty Required
    description: x
    pr_origin: "#2"
    paths:
      - empty_required_dir
    optional: false
`)
	m, err := LoadManifest(mp)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	_, err = Build(m, BundleOptions{
		RepoRoot:     repoRoot,
		ManifestPath: mp,
		CommitSHA:    "deadbeef",
		SkipMake:     true,
	})
	if err == nil {
		t.Fatal("expected error when required component contributes zero files, got nil")
	}
	if !strings.Contains(err.Error(), "empty_req") || !strings.Contains(err.Error(), "zero files") {
		t.Errorf("error should mention component id + zero-files; got: %v", err)
	}
}

func TestLoadManifest_RejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "m.yaml")
	mustWrite(t, p, `
version: 99
bundle_name: x
output_dir: out
components:
  - id: a
    title: A
    description: x
    pr_origin: x
    paths: [a]
    optional: false
`)
	_, err := LoadManifest(p)
	if err == nil {
		t.Fatal("expected error for unsupported version, got nil")
	}
}

// readTarball reads a gzip-compressed tarball and returns a map
// of <name-without-bundle-prefix> -> file body. The bundle prefix
// (e.g. `synth-bundle-2026-05-30-deadbee/`) is stripped so the
// test asserts against logical bundle-relative paths.
func readTarball(t *testing.T, path string) map[string]string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open tarball: %v", err)
	}
	defer f.Close()
	gzr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gzr.Close()
	tr := tar.NewReader(gzr)
	out := make(map[string]string)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar.Next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		// strip the bundle-stem prefix
		parts := strings.SplitN(hdr.Name, "/", 2)
		if len(parts) != 2 {
			continue
		}
		out[parts[1]] = string(body)
	}
	return out
}

func contains(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
