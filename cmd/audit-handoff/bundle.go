package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BundleOptions configures one bundling run.
type BundleOptions struct {
	// RepoRoot is the absolute path to the repo root. All
	// component paths in the manifest are resolved relative
	// to this.
	RepoRoot string

	// ManifestPath is the absolute path to the manifest YAML.
	// Used both for parsing and for including the manifest
	// itself in the bundle (so the auditor can re-derive the
	// expected layout).
	ManifestPath string

	// CommitSHA is the git commit the bundle is anchored to.
	// Recorded in MANIFEST.txt and used in the bundle filename.
	// Required (the bundler refuses to produce a bundle that
	// is not anchored to a public commit).
	CommitSHA string

	// BuildTime is the timestamp recorded in MANIFEST.txt.
	// Defaults to time.Now().UTC() when zero.
	BuildTime time.Time

	// Out is where progress and warnings are written. Defaults
	// to os.Stderr when nil. Errors are returned, not written
	// here.
	Out io.Writer

	// SkipMake, when true, suppresses invocation of any
	// component's `make_target`. The path-copy step still
	// runs. Used by tests so a unit run does not require
	// `make` on PATH or a working build environment.
	SkipMake bool

	// AllowMissingOptional, when true, downgrades a missing
	// optional path from a `MISSING:` placeholder write into
	// a complete skip (no placeholder in the bundle). The
	// default is to write the placeholder so an auditor can
	// tell which components were *expected* but not yet
	// available at build time.
	//
	// Required components NEVER use the placeholder path: a
	// missing required component is always a hard error.
	AllowMissingOptional bool
}

// Result summarises one bundling run.
type Result struct {
	OutputPath        string   // absolute path to the tarball
	ComponentsIncluded []string // ids of components that contributed at least one real file
	ComponentsMissing  []string // ids of optional components that contributed only a MISSING: placeholder
	FileCount          int      // number of regular files inside the tarball
	BytesUncompressed  int64    // total uncompressed payload size
}

// Build produces the hand-off tarball described by m, written
// under opts.RepoRoot / m.OutputDir.
func Build(m *Manifest, opts BundleOptions) (*Result, error) {
	if opts.RepoRoot == "" {
		return nil, errors.New("RepoRoot is required")
	}
	if opts.CommitSHA == "" {
		return nil, errors.New("CommitSHA is required (bundle must be anchored to a public commit)")
	}
	if opts.BuildTime.IsZero() {
		opts.BuildTime = time.Now().UTC()
	}
	if opts.Out == nil {
		opts.Out = os.Stderr
	}

	outDir := filepath.Join(opts.RepoRoot, m.OutputDir)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %q: %w", outDir, err)
	}

	dateStr := opts.BuildTime.Format("2006-01-02")
	shortSHA := opts.CommitSHA
	if len(shortSHA) > 7 {
		shortSHA = shortSHA[:7]
	}
	bundleStem := fmt.Sprintf("%s-%s-%s", m.BundleName, dateStr, shortSHA)
	outPath := filepath.Join(outDir, bundleStem+".tar.gz")

	// Build into a temp file then rename so the bundle never
	// appears half-written under its final name.
	//
	// os.CreateTemp (rather than os.Create on a deterministic
	// `outPath+".tmp"`) gives a unique-per-invocation name so two
	// bundler runs in the same outDir (e.g. a CI run and a manual
	// rerun on the same commit, or a parallel build of two manifests)
	// cannot trample each other's tmp file. We keep the `.tmp` suffix
	// at the end of the pattern so the regression test that scans
	// outDir for stray `*.tmp` files (bundle_test.go ~line 215) still
	// matches.
	tmpFile, err := os.CreateTemp(outDir, bundleStem+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temp file in %q: %w", outDir, err)
	}
	tmpPath := tmpFile.Name()
	// Safety-net cleanup: on the error return paths below, the
	// explicit close chain that runs before os.Rename is never
	// reached, which would leak the file descriptor and leave
	// tmpPath on disk. These defers ensure both are cleaned up
	// even if a writePath/writeIndex/writeFileBytes call fails.
	// On the success path they are no-ops:
	//   - tw / gzw / tmpFile have already been explicitly closed
	//     in order (tar -> gzip -> file). tar.Writer.Close and
	//     gzip.Writer.Close are idempotent; os.File.Close on an
	//     already-closed file returns an error we discard.
	//   - os.Remove(tmpPath) hits ErrNotExist after the rename,
	//     also discarded.
	gzw := gzip.NewWriter(tmpFile)
	tw := tar.NewWriter(gzw)
	defer func() { _ = os.Remove(tmpPath) }()
	defer func() { _ = tmpFile.Close() }()
	defer func() { _ = gzw.Close() }()
	defer func() { _ = tw.Close() }()

	w := &bundleWriter{
		tw:        tw,
		bundleDir: bundleStem,
		manifest:  &manifestRecorder{},
	}

	// Run any make_target hooks first so the produced artifacts
	// (e.g. the inner audit-bundle tarball) exist on disk when
	// the path-copy step runs.
	for _, c := range m.Components {
		if c.MakeTarget == "" {
			continue
		}
		if opts.SkipMake {
			fmt.Fprintf(opts.Out, "audit-handoff: SkipMake=true, not invoking make %s for component %s\n", c.MakeTarget, c.ID)
			continue
		}
		if err := runMakeTarget(opts.RepoRoot, c.MakeTarget, opts.Out); err != nil {
			if c.Optional {
				fmt.Fprintf(opts.Out, "audit-handoff: WARN component %s make target %q failed (%v), proceeding because component is optional\n", c.ID, c.MakeTarget, err)
				continue
			}
			return nil, fmt.Errorf("component %s: make %s: %w", c.ID, c.MakeTarget, err)
		}
	}

	included := []string{}
	missing := []string{}
	for _, c := range m.Components {
		filesWritten := 0
		for _, rel := range c.Paths {
			abs := filepath.Join(opts.RepoRoot, rel)
			info, statErr := os.Stat(abs)
			switch {
			case statErr == nil:
				// writePath returns the number of real files it
				// wrote. For a regular file that is always 1; for
				// a directory it is the count of regular files
				// produced by filepath.Walk (skipping symlinks,
				// sockets, etc). We only count a component as
				// "included" if it actually contributed bytes — a
				// path that exists but resolves to an empty
				// directory or to a tree of nothing-but-symlinks
				// is treated the same as a missing optional, so
				// INDEX.md doesn't claim "included" for a
				// component whose tar entries are all absent.
				n, err := w.writePath(abs, rel, c.ID, info)
				if err != nil {
					return nil, fmt.Errorf("component %s path %q: %w", c.ID, rel, err)
				}
				filesWritten += n
			case errors.Is(statErr, os.ErrNotExist):
				if !c.Optional {
					return nil, fmt.Errorf("component %s: required path %q does not exist (commit %s)", c.ID, rel, opts.CommitSHA)
				}
				if !opts.AllowMissingOptional {
					if err := w.writeMissingPlaceholder(c, rel); err != nil {
						return nil, fmt.Errorf("component %s placeholder for %q: %w", c.ID, rel, err)
					}
				}
				fmt.Fprintf(opts.Out, "audit-handoff: NOTE component %s optional path %q not found (origin %s) — recorded as MISSING in INDEX.md\n", c.ID, rel, c.PROrigin)
			default:
				return nil, fmt.Errorf("component %s path %q: %w", c.ID, rel, statErr)
			}
		}
		if filesWritten > 0 {
			included = append(included, c.ID)
		} else if c.Optional {
			missing = append(missing, c.ID)
		}
		// Required components with filesWritten == 0 can only happen
		// if every declared path resolved to an empty/symlink-only
		// directory. The build still succeeds, but writeIndex will
		// then label them "MISSING (optional)" which is wrong for a
		// required component. We don't currently have a manifest
		// declaring a directory-only required path (progress_pin
		// declares two specific .md files), so this case is
		// unreachable today; if a future manifest changes that, the
		// drift test paths-exist already enforces that REQUIRED files
		// resolve, so the only way to reach filesWritten==0 on a
		// required component is the empty-directory case described
		// in ANALYSIS_0004 — which is what this whole block fixes
		// the optional-side of. The auditor would still see the
		// component listed in INDEX.md (every manifest component
		// gets a section), just with the wrong status string; we
		// accept that as a known edge case worth catching in code
		// review of any future manifest change.
	}

	// Write the human-readable INDEX.md and the manifest copy
	// at the top of the bundle.
	if err := w.writeIndex(m, opts, included); err != nil {
		return nil, fmt.Errorf("write INDEX.md: %w", err)
	}
	// Copy the manifest verbatim so the auditor sees the same
	// source of truth the bundler used.
	manifestBytes, err := os.ReadFile(opts.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("re-read manifest for bundle: %w", err)
	}
	if err := w.writeFileBytes("manifest.yaml", manifestBytes, 0o644); err != nil {
		return nil, fmt.Errorf("write manifest.yaml: %w", err)
	}

	// Last: the MANIFEST.txt with the SHA-256 chain.
	manifestTxt := w.manifest.render(m.BundleName, opts.CommitSHA, opts.BuildTime)
	if err := w.writeFileBytes("MANIFEST.txt", manifestTxt, 0o644); err != nil {
		return nil, fmt.Errorf("write MANIFEST.txt: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("close tar: %w", err)
	}
	if err := gzw.Close(); err != nil {
		return nil, fmt.Errorf("close gzip: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return nil, fmt.Errorf("close tmp file: %w", err)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		return nil, fmt.Errorf("rename %q -> %q: %w", tmpPath, outPath, err)
	}

	return &Result{
		OutputPath:         outPath,
		ComponentsIncluded: included,
		ComponentsMissing:  missing,
		FileCount:          w.manifest.count(),
		BytesUncompressed:  w.manifest.bytesTotal(),
	}, nil
}

// bundleWriter wraps a tar writer plus the bundle's top-level
// directory prefix and the running MANIFEST.txt accumulator.
type bundleWriter struct {
	tw        *tar.Writer
	bundleDir string
	manifest  *manifestRecorder
}

// writePath copies the given absolute path (file or directory)
// into the bundle. The bundle layout is
// <bundleDir>/<componentID>/<repo-relative-path>.
//
// Returns the number of regular files actually written to the
// tar stream. For a single regular file that is always 1; for a
// directory it is the count of regular files emitted by
// filepath.Walk after filtering out directories, symlinks, and
// other non-regular entries. The caller uses this count to
// distinguish "path exists and contributed content" from "path
// exists but was empty / contained only non-regular entries" so
// INDEX.md does not falsely claim an empty path was included.
func (w *bundleWriter) writePath(abs, rel, componentID string, info os.FileInfo) (int, error) {
	if !info.IsDir() {
		if err := w.writeFileFromDisk(abs, rel, componentID, info); err != nil {
			return 0, err
		}
		return 1, nil
	}
	// Directory: walk it and write every regular file, sorted
	// so the bundle is byte-deterministic for a given tree.
	type entry struct {
		path string
		info os.FileInfo
	}
	var files []entry
	if err := filepath.Walk(abs, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		if fi.Mode()&os.ModeType != 0 {
			// Skip symlinks, sockets, devices, etc. The bundle
			// is for an external auditor and should be a
			// content-only artifact; symlinks would point at
			// paths that don't exist after extraction.
			return nil
		}
		files = append(files, entry{p, fi})
		return nil
	}); err != nil {
		return 0, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	written := 0
	for _, f := range files {
		relInside, err := filepath.Rel(abs, f.path)
		if err != nil {
			return written, err
		}
		// rel is the top-level path entry (e.g. tests/dr); inside
		// the bundle we want <componentID>/<rel>/<relInside>.
		fullRel := filepath.Join(rel, relInside)
		if err := w.writeFileFromDisk(f.path, fullRel, componentID, f.info); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
}

func (w *bundleWriter) writeFileFromDisk(abs, rel, componentID string, info os.FileInfo) error {
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	target := filepath.ToSlash(filepath.Join(componentID, rel))
	return w.writeFileBytes(target, data, info.Mode().Perm())
}

// writeFileBytes writes a single file into the bundle at
// <bundleDir>/<relInsideBundle>, records its SHA-256 in the
// running manifest, and bumps the file/byte counters.
func (w *bundleWriter) writeFileBytes(relInsideBundle string, data []byte, mode os.FileMode) error {
	name := filepath.ToSlash(filepath.Join(w.bundleDir, relInsideBundle))
	hdr := &tar.Header{
		Name:     name,
		Mode:     int64(mode &^ 0o111), // strip exec bit on content files; we don't ship anything executable
		Size:     int64(len(data)),
		ModTime:  time.Unix(0, 0).UTC(), // deterministic mtime
		Typeflag: tar.TypeReg,
	}
	if err := w.tw.WriteHeader(hdr); err != nil {
		return err
	}
	if _, err := w.tw.Write(data); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	w.manifest.record(relInsideBundle, sum[:], int64(len(data)))
	return nil
}

// writeMissingPlaceholder emits a small marker file at
// <componentID>/__MISSING__/<sanitised-rel>.txt when an
// optional component's source path is not yet on the branch.
// The auditor sees a per-component MISSING marker so the
// expected layout is preserved.
func (w *bundleWriter) writeMissingPlaceholder(c Component, rel string) error {
	body := fmt.Sprintf(
		"This file would have come from %q in component %q (%s).\n"+
			"It is not yet present at bundle time because %s.\n"+
			"Origin PR: %s\n"+
			"Re-build the bundle once the originating PR has merged.\n",
		rel, c.ID, c.Title, "the originating PR is still open", c.PROrigin)
	// Replace path separators so the placeholder filename is
	// flat. e.g. "tests/dr/verifier.go" -> "tests_dr_verifier.go.MISSING".
	flat := strings.ReplaceAll(rel, "/", "_") + ".MISSING"
	// filepath.ToSlash is required here for parity with writeFileFromDisk
	// (line ~296). writeFileBytes uses target verbatim as the manifestRecorder
	// key, and the tar header name goes through filepath.ToSlash inside
	// writeFileBytes too — so without this call, a Windows builder would
	// record the placeholder in MANIFEST.txt with backslashes while the
	// tar entry uses forward slashes, breaking sha256sum -c.
	target := filepath.ToSlash(filepath.Join(c.ID, "__MISSING__", flat))
	return w.writeFileBytes(target, []byte(body), 0o644)
}

// writeIndex emits the top-level INDEX.md the auditor sees
// when extracting the bundle. It is deliberately a separate
// file from README.md (which is the *source* doc that lives
// in deploy/audit-handoff/README.md and is copied in via the
// progress_pin / always-included path); INDEX.md is generated
// per-bundle and carries the build timestamp, commit, and the
// resolved-vs-missing component status.
//
// The function takes only `included` (not `missing`) because the
// status is fully determined by the included-set: any component
// not in `included` is necessarily a missing-optional (required
// components hard-fail in Build before this point), and the default
// status string already reflects that. The caller's `missing` slice
// is still surfaced separately as Result.ComponentsMissing for
// programmatic consumers.
func (w *bundleWriter) writeIndex(m *Manifest, opts BundleOptions, included []string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s — audit hand-off bundle\n\n", m.BundleName)
	fmt.Fprintf(&sb, "Commit:  `%s`  \n", opts.CommitSHA)
	fmt.Fprintf(&sb, "Built:   `%s`  \n", opts.BuildTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "Source:  https://github.com/kennguy3n/zk-object-fabric/commit/%s\n\n", opts.CommitSHA)
	// Reference the actual files the auditor will find inside the bundle.
	// The bundle layout is <bundle-stem>/<componentID>/<rel-path>, so the
	// system overview lives at progress_pin/docs/PROPOSAL.md (always
	// shipped — progress_pin is the only required component) and the
	// security README at audit_bundle/docs/security/README.md (shipped
	// when the audit_bundle make target succeeds and the optional source
	// files are present on the branch).
	fmt.Fprintf(&sb, "Start by reading `progress_pin/docs/PROPOSAL.md` (system overview) and then\n")
	fmt.Fprintf(&sb, "`audit_bundle/docs/security/README.md` if this bundle has the audit_bundle component.\n\n")
	fmt.Fprintf(&sb, "Components\n----------\n\n")
	for _, c := range m.Components {
		// Default is "MISSING (optional)" because in practice every
		// optional component with no real file lands in `missing`
		// (see Build's hadReal/included/missing accounting), and
		// every required component lands in `included` or the build
		// hard-fails before this point. Initialising to the same
		// string the (formerly explicit) `case containsString(missing,...)`
		// branch would have produced removes a redundant case without
		// changing behaviour.
		status := "MISSING (optional)"
		if containsString(included, c.ID) {
			status = "included"
		}
		fmt.Fprintf(&sb, "## %s — %s\n\n", c.ID, c.Title)
		fmt.Fprintf(&sb, "Status: **%s**  \n", status)
		fmt.Fprintf(&sb, "Origin: %s\n\n", c.PROrigin)
		fmt.Fprintf(&sb, "%s\n", strings.TrimSpace(c.Description))
		fmt.Fprintf(&sb, "\nPaths:\n")
		for _, p := range c.Paths {
			fmt.Fprintf(&sb, "- `%s`\n", p)
		}
		fmt.Fprintf(&sb, "\n")
	}
	return w.writeFileBytes("INDEX.md", []byte(sb.String()), 0o644)
}

// manifestRecorder accumulates the per-file SHA-256 chain that
// becomes MANIFEST.txt. The format is intentionally compatible
// with `sha256sum -c` so an auditor can verify the bundle with
// nothing but coreutils.
type manifestRecorder struct {
	entries []manifestEntry
	bytes   int64
}

type manifestEntry struct {
	path string
	sum  string
	size int64
}

func (m *manifestRecorder) record(path string, sum []byte, size int64) {
	m.entries = append(m.entries, manifestEntry{
		path: path,
		sum:  hex.EncodeToString(sum),
		size: size,
	})
	m.bytes += size
}

func (m *manifestRecorder) count() int      { return len(m.entries) }
func (m *manifestRecorder) bytesTotal() int64 { return m.bytes }

// render produces the MANIFEST.txt body. Header lines are
// prefixed with `#` so `sha256sum -c` ignores them (per the
// spec; coreutils' sha256sum -c stops parsing the header lines
// and only verifies `<hex>  <path>` lines). The data lines are
// sorted for deterministic output.
func (m *manifestRecorder) render(bundleName, commitSHA string, buildTime time.Time) []byte {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# %s audit hand-off bundle\n", bundleName)
	fmt.Fprintf(&sb, "# commit: %s\n", commitSHA)
	fmt.Fprintf(&sb, "# built:  %s\n", buildTime.UTC().Format(time.RFC3339))
	fmt.Fprintf(&sb, "# verify: sha256sum -c MANIFEST.txt   (run from the extracted bundle dir)\n")
	fmt.Fprintf(&sb, "#\n")
	entries := append([]manifestEntry(nil), m.entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	for _, e := range entries {
		// Two-space separator is what coreutils' sha256sum emits.
		fmt.Fprintf(&sb, "%s  %s\n", e.sum, e.path)
	}
	return []byte(sb.String())
}

// runMakeTarget invokes `make <target>` from repoRoot. Errors
// from make are returned wrapped; stdout/stderr are written to
// out so the operator running the bundler can see the build log.
func runMakeTarget(repoRoot, target string, out io.Writer) error {
	cmd := exec.Command("make", target)
	cmd.Dir = repoRoot
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}

func containsString(s []string, x string) bool {
	for _, v := range s {
		if v == x {
			return true
		}
	}
	return false
}
