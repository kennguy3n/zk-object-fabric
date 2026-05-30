// Package main contains the audit-handoff bundler — the CLI that
// reads deploy/audit-handoff/manifest.yaml and emits a single
// tarball an external auditor receives. The manifest is the
// single source of truth for what the bundle contains; this file
// defines its schema and load semantics.
//
// The manifest is intentionally a plain YAML file (not a Go
// literal) so that it can be edited by anyone landing a new
// workstream without having to recompile the bundler. The drift
// tests in tests/audit/handoff_test.go assert three invariants:
//
//   1. Every component listed in the manifest has at least one
//      `paths:` entry that resolves on disk, OR is explicitly
//      marked `optional: true` because its source PR has not
//      landed yet.
//
//   2. Every component listed in the manifest is mentioned by
//      its `id:` (or `title:`) in deploy/audit-handoff/README.md.
//
//   3. Every component reference in deploy/audit-handoff/README.md
//      resolves to a real `id:` in the manifest (no stale or
//      typo'd references).
//
// Adding a new component is therefore strictly: edit YAML, edit
// README, run `go test ./tests/audit/... ./cmd/audit-handoff/...`.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Manifest is the top-level schema of deploy/audit-handoff/manifest.yaml.
type Manifest struct {
	// Version pins the schema. Increment on incompatible changes
	// (e.g. renaming or removing a field). The bundler refuses
	// to operate on a version it does not recognise so a stale
	// CLI cannot silently produce a malformed bundle from a
	// newer manifest.
	Version int `yaml:"version"`

	// BundleName is the leading segment of the output filename
	// (e.g. `zkof-audit-handoff` produces
	// `zkof-audit-handoff-<DATE>-<COMMIT>.tar.gz`).
	BundleName string `yaml:"bundle_name"`

	// OutputDir is the directory under the repo root where the
	// tarball is written. Defaults to `build/audit-handoff` when
	// empty. Always relative to the repo root.
	OutputDir string `yaml:"output_dir"`

	// Components is the list of artifact groups that get bundled.
	// Order is significant: the bundler writes components in
	// declared order, so the auditor sees them in the same order
	// the README references them.
	Components []Component `yaml:"components"`
}

// Component is one artifact group in the bundle.
type Component struct {
	// ID is a short slug that uniquely identifies the component.
	// Referenced by the drift tests; must match `^[a-z][a-z0-9_]*$`.
	// The README references components by this slug (case-insensitive
	// or by `Title`).
	ID string `yaml:"id"`

	// Title is the human-readable name shown to the auditor in
	// the bundle's top-level INDEX.md.
	Title string `yaml:"title"`

	// Description is shown verbatim in the bundle's INDEX.md
	// below the component title. Markdown is permitted.
	Description string `yaml:"description"`

	// PROrigin is a free-form reference to the GitHub PR(s) that
	// introduced this component. Used for traceability; not
	// parsed by the bundler.
	PROrigin string `yaml:"pr_origin"`

	// Paths is the list of repo-relative paths that get copied
	// into the bundle under `<bundle>/<component_id>/`. Each
	// path is either a file or a directory; directories are
	// copied recursively.
	Paths []string `yaml:"paths"`

	// MakeTarget, when non-empty, names a top-level Makefile
	// target that the bundler invokes BEFORE copying `Paths`.
	// This is used for components whose physical artifact is
	// produced by a Make target rather than already-existing
	// repo files (e.g. `audit-bundle` produces a tarball that
	// is then nested inside the hand-off bundle).
	//
	// The bundler invokes `make <target>` from the repo root
	// with the same environment it inherited; if Make is not
	// available or the target fails, the bundler treats it
	// the same as a missing optional path (warn + continue
	// for optional components, hard-fail for required ones).
	MakeTarget string `yaml:"make_target,omitempty"`

	// Optional marks a component as one whose source PR has
	// not yet landed on `main`. The bundler emits a `MISSING:`
	// placeholder for missing paths instead of failing. The
	// drift tests skip the existence check for optional
	// components with a clear log message identifying the
	// originating PR.
	//
	// `progress_pin` is the only required component (it always
	// exists on every branch); every WS-1.x and WS-2.x output
	// is optional until merged.
	Optional bool `yaml:"optional"`
}

// LoadManifest reads and parses the manifest at the given path.
// All fields are validated for basic well-formedness before
// returning. Returns a wrapped error if the file is missing,
// the YAML is malformed, or any required field is empty.
func LoadManifest(path string) (*Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest %q: %w", path, err)
	}
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // catch typos in field names
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("parse manifest %q: %w", path, err)
	}
	if err := m.validate(path); err != nil {
		return nil, err
	}
	return &m, nil
}

// validate runs structural checks on the loaded manifest. It
// catches issues that the YAML parser doesn't catch by itself
// (e.g. empty required fields, duplicate component IDs).
func (m *Manifest) validate(path string) error {
	if m.Version != 1 {
		return fmt.Errorf("manifest %q: unsupported version %d (this bundler supports version 1)", path, m.Version)
	}
	if m.BundleName == "" {
		return fmt.Errorf("manifest %q: bundle_name is required", path)
	}
	// OutputDir defaults to build/audit-handoff when empty.
	if m.OutputDir == "" {
		m.OutputDir = "build/audit-handoff"
	}
	if filepath.IsAbs(m.OutputDir) {
		return fmt.Errorf("manifest %q: output_dir must be relative to the repo root (got %q)", path, m.OutputDir)
	}
	if len(m.Components) == 0 {
		return fmt.Errorf("manifest %q: at least one component is required", path)
	}
	seen := make(map[string]int, len(m.Components))
	hasRequired := false
	for i, c := range m.Components {
		if c.ID == "" {
			return fmt.Errorf("manifest %q component[%d]: id is required", path, i)
		}
		if !validID(c.ID) {
			return fmt.Errorf("manifest %q component[%d]: id %q must match ^[a-z][a-z0-9_]*$", path, i, c.ID)
		}
		if prev, dup := seen[c.ID]; dup {
			return fmt.Errorf("manifest %q component[%d]: duplicate id %q (also at component[%d])", path, i, c.ID, prev)
		}
		seen[c.ID] = i
		if c.Title == "" {
			return fmt.Errorf("manifest %q component[%d] (%s): title is required", path, i, c.ID)
		}
		if len(c.Paths) == 0 {
			return fmt.Errorf("manifest %q component[%d] (%s): at least one path is required", path, i, c.ID)
		}
		for j, p := range c.Paths {
			if filepath.IsAbs(p) {
				return fmt.Errorf("manifest %q component[%d] (%s) path[%d]: must be relative to the repo root (got %q)", path, i, c.ID, j, p)
			}
		}
		if !c.Optional {
			hasRequired = true
		}
	}
	if !hasRequired {
		// Defence-in-depth: if every component is optional, a
		// freshly-cut branch could produce a bundle with zero
		// content and still pass the drift test. Require at
		// least one always-present component so the bundle
		// always has something concrete in it (today this is
		// `progress_pin`, which references docs/PROGRESS.md
		// and docs/PROPOSAL.md — files that exist on every
		// branch).
		return fmt.Errorf("manifest %q: at least one component must be non-optional (so the bundle always has anchored content)", path)
	}
	return nil
}

// validID matches `^[a-z][a-z0-9_]*$`. We don't import regexp
// for this — it's a hot enough check that a small hand-rolled
// scan is faster and avoids the dependency.
func validID(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			// always allowed
		case (r >= '0' && r <= '9') || r == '_':
			if i == 0 {
				return false // must start with a letter
			}
		default:
			return false
		}
	}
	return true
}


