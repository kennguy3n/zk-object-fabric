// Package external aggregates third-party S3 conformance harness
// outputs (Ceph s3-tests XUnit XML and MinIO mint per-SDK JSON logs)
// into a single normalised conformance matrix consumable by the
// audit dossier.
//
// The package is intentionally independent of the gateway and the
// in-process runner in tests/s3_conformance: it only needs the
// upstream harness output files. This makes the aggregation step
// reproducible from a checked-in dossier artifact set without
// re-running the harnesses, which is exactly what an auditor wants
// when they need to re-validate a published matrix.
//
// Each upstream harness has a different output schema:
//
//   - Ceph s3-tests is nose-based and emits a standard JUnit XML
//     (`--with-xunit --xunit-file=...`). A <testcase> child of
//     <failure> is a real defect; <error> is an infrastructure
//     fault; <skipped> is an intentional skip from the
//     `-a '!skip-tag'` selector. The skip list is the
//     authoritative mapping to the gateway's "unsupported" set —
//     skipped tests become MatrixEntry.Status=unsupported.
//
//   - MinIO mint emits one newline-delimited JSON log per SDK
//     ('mint-logs/{date}/{sdk}/log.json'). Each line has a `name`
//     (sdk), `function` (S3 op), and `status` ("PASS" / "FAIL" /
//     "NA"). FAIL is a defect; NA means the SDK does not support
//     the test (counted as unsupported); PASS is a pass.
//
// Both schemas collapse onto a small set of normalised statuses:
//
//	OpPassed       — server matched AWS S3 reference behaviour.
//	OpFailed       — server returned a divergent response; defect.
//	OpUnsupported  — harness skip / NA; documented gap.
//	OpErrored      — infrastructure fault; triage as ops issue.
//
// The aggregated matrix matches the field shape produced by the
// in-process tests/s3_conformance Runner so an auditor can diff the
// internal and external matrices for the same gateway build and
// confirm there is no surface area the internal runner believes is
// supported but the external harnesses disagree on. (And vice
// versa, which catches the in-process runner being too lenient.)
package external

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// OpStatus is the normalised per-operation outcome. The value
// space matches tests/s3_conformance.OpStatus verbatim so the
// internal and external matrices can be diffed without a mapping
// table.
type OpStatus string

const (
	// OpPassed: harness recorded a pass.
	OpPassed OpStatus = "passed"
	// OpFailed: harness recorded a failure against AWS reference
	// behaviour. Defect — must triage.
	OpFailed OpStatus = "failed"
	// OpUnsupported: harness recorded an intentional skip (s3-tests
	// `!tag`) or NA (mint). Documented gap, not a defect.
	OpUnsupported OpStatus = "unsupported"
	// OpErrored: harness could not run the test to a verdict
	// (infrastructure fault, missing fixture, harness panic).
	OpErrored OpStatus = "errored"
)

// Source identifies which upstream harness produced an entry.
// Audit consumers use this to attribute failures to the right
// vendor when filing bug reports back upstream (e.g. an s3-tests
// failure that is a known-bad assertion against AWS itself goes
// to Ceph, not to us).
type Source string

const (
	// SourceCephS3Tests indicates the entry came from a Ceph
	// s3-tests JUnit XML.
	SourceCephS3Tests Source = "ceph-s3-tests"
	// SourceMinioMint indicates the entry came from a MinIO mint
	// per-SDK JSON log.
	SourceMinioMint Source = "minio-mint"
)

// MatrixEntry is one row of the aggregated conformance matrix.
// It is intentionally a flat shape so the JSON serialisation
// diffs cleanly against the in-process matrix.
type MatrixEntry struct {
	// Op is the harness-reported operation or test name. For
	// s3-tests this is the test method (e.g.
	// "test_bucket_create_naming_good"). For mint this is the SDK
	// function name (e.g. "PutObject").
	Op string `json:"op"`

	// Category groups related ops in the rendered matrix.
	// s3-tests uses the nose class (e.g. "test_s3"); mint uses
	// the mode-or-category (e.g. "core", "presigned").
	Category string `json:"category"`

	// Source identifies the upstream harness.
	Source Source `json:"source"`

	// SDK is the language SDK the test exercises. Empty for
	// s3-tests (Python only); for mint, one of "aws-sdk-go",
	// "aws-sdk-java", "aws-sdk-python", "aws-sdk-js",
	// "minio-go", "minio-java", "minio-py", "minio-js", etc.
	SDK string `json:"sdk,omitempty"`

	// Status is the normalised outcome.
	Status OpStatus `json:"status"`

	// DurationMs is the harness-reported wall-clock duration.
	// Zero if the harness did not record it.
	DurationMs int64 `json:"duration_ms,omitempty"`

	// Detail is a short human-readable summary attached to
	// failed/unsupported/errored rows. Empty on pass.
	Detail string `json:"detail,omitempty"`
}

// Matrix is the aggregated multi-harness conformance matrix.
type Matrix struct {
	// GatewayEndpoint identifies the gateway the harnesses ran
	// against. Stamped into the matrix so the dossier can record
	// which deployment produced the result set.
	GatewayEndpoint string `json:"gateway_endpoint,omitempty"`
	// GatewaySHA is the commit SHA of the gateway binary the
	// harnesses ran against. Stamped from the operator's run
	// script ($GATEWAY_SHA).
	GatewaySHA string `json:"gateway_sha,omitempty"`
	// GeneratedAt is the UTC time the aggregation was performed.
	GeneratedAt time.Time `json:"generated_at"`
	// Entries are sorted by (Source, SDK, Category, Op) for
	// stable diffs across runs.
	Entries []MatrixEntry `json:"entries"`
}

// Counts returns a tally of statuses across the matrix.
type Counts struct {
	Passed      int `json:"passed"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
	Errored     int `json:"errored"`
	Total       int `json:"total"`
}

// Counts walks Entries and returns the per-status tally.
func (m Matrix) Counts() Counts {
	var c Counts
	for _, e := range m.Entries {
		c.Total++
		switch e.Status {
		case OpPassed:
			c.Passed++
		case OpFailed:
			c.Failed++
		case OpUnsupported:
			c.Unsupported++
		case OpErrored:
			c.Errored++
		}
	}
	return c
}

// AllPassed returns true iff the matrix has at least one entry and
// every entry is either Passed or Unsupported (i.e. zero Failed AND
// zero Errored). The Total > 0 precondition matters because this
// gate decides whether to publish an audit matrix: an operator who
// accidentally points the aggregator at an empty directory must not
// receive a silent audit-pass. Callers that legitimately accept a
// zero-entry matrix (e.g. partial-harness re-aggregations during
// incident triage) should inspect Counts() directly.
func (m Matrix) AllPassed() bool {
	c := m.Counts()
	return c.Total > 0 && c.Failed == 0 && c.Errored == 0
}

// WriteJSON writes m as deterministic, indented JSON. It sorts a
// local copy of Entries rather than the caller's underlying slice
// — a value receiver in Go conventionally signals no mutation, so
// sorting m.Entries in place would silently reorder a slice the
// caller still holds a reference to. The copy is cheap (entry
// structs are small and the matrix is at most a few thousand rows).
func (m Matrix) WriteJSON(w io.Writer) error {
	sorted := slices.Clone(m.Entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Source != b.Source {
			return a.Source < b.Source
		}
		if a.SDK != b.SDK {
			return a.SDK < b.SDK
		}
		if a.Category != b.Category {
			return a.Category < b.Category
		}
		return a.Op < b.Op
	})
	out := Matrix{
		GatewayEndpoint: m.GatewayEndpoint,
		GatewaySHA:      m.GatewaySHA,
		GeneratedAt:     m.GeneratedAt,
		Entries:         sorted,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// ParseS3TestsXUnit parses a Ceph s3-tests --with-xunit XML file
// into MatrixEntry rows.
//
// XUnit schema (subset relevant here):
//
//	<testsuite>
//	  <testcase classname="..." name="..." time="...">
//	    [optional one of <failure>/<error>/<skipped>]
//	  </testcase>
//	</testsuite>
//
// A <testcase> with no inner element is a pass; <failure> is a
// real defect; <error> is an infrastructure fault; <skipped> is
// an intentional skip from the `-a '!tag'` selector and maps to
// the gateway's "unsupported" set.
func ParseS3TestsXUnit(r io.Reader) ([]MatrixEntry, error) {
	type inner struct {
		Type    string `xml:"type,attr"`
		Message string `xml:"message,attr"`
		Body    string `xml:",chardata"`
	}
	type tc struct {
		ClassName string  `xml:"classname,attr"`
		Name      string  `xml:"name,attr"`
		Time      float64 `xml:"time,attr"`
		Failure   *inner  `xml:"failure"`
		Error     *inner  `xml:"error"`
		Skipped   *inner  `xml:"skipped"`
	}
	type ts struct {
		XMLName   xml.Name `xml:"testsuite"`
		TestCases []tc     `xml:"testcase"`
	}
	type tss struct {
		XMLName xml.Name `xml:"testsuites"`
		Suites  []ts     `xml:"testsuite"`
	}

	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read xunit: %w", err)
	}
	// Try wrapped <testsuites> first (newer nose / pytest-xdist
	// emit this), then plain <testsuite>.
	var (
		suites []ts
		multi  tss
	)
	if err := xml.Unmarshal(body, &multi); err == nil && len(multi.Suites) > 0 {
		suites = multi.Suites
	} else {
		var single ts
		if err := xml.Unmarshal(body, &single); err != nil {
			return nil, fmt.Errorf("parse xunit: %w", err)
		}
		suites = []ts{single}
	}

	var out []MatrixEntry
	for _, s := range suites {
		for _, c := range s.TestCases {
			e := MatrixEntry{
				Op:         c.Name,
				Category:   shortCategory(c.ClassName),
				Source:     SourceCephS3Tests,
				DurationMs: int64(c.Time * 1000),
			}
			switch {
			case c.Failure != nil:
				e.Status = OpFailed
				e.Detail = compactDetail(c.Failure.Type, c.Failure.Message, c.Failure.Body)
			case c.Error != nil:
				e.Status = OpErrored
				e.Detail = compactDetail(c.Error.Type, c.Error.Message, c.Error.Body)
			case c.Skipped != nil:
				e.Status = OpUnsupported
				e.Detail = compactDetail(c.Skipped.Type, c.Skipped.Message, c.Skipped.Body)
			default:
				e.Status = OpPassed
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// MintLogEntry is one line of a MinIO mint per-SDK log.json file.
// Field tags match the upstream schema documented at
// https://github.com/minio/mint#about-mint-output exactly so a
// future mint version that adds new fields can still be
// unmarshalled without code changes.
type MintLogEntry struct {
	Name     string `json:"name"`     // SDK / harness name (e.g. "aws-sdk-go")
	Function string `json:"function"` // S3 op or test method
	Duration int64  `json:"duration"` // milliseconds
	Status   string `json:"status"`   // "PASS" / "FAIL" / "NA"
	Alert    string `json:"alert,omitempty"`
	Message  string `json:"message,omitempty"`
	Error    string `json:"error,omitempty"`
}

// ParseMintLog parses a single MinIO mint log.json file
// (newline-delimited JSON) into MatrixEntry rows. The category
// is derived from the SDK name (mint groups tests per-SDK at the
// directory level, not in the log).
func ParseMintLog(r io.Reader) ([]MatrixEntry, error) {
	dec := json.NewDecoder(r)
	var out []MatrixEntry
	for {
		var le MintLogEntry
		if err := dec.Decode(&le); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("parse mint log: %w", err)
		}
		e := MatrixEntry{
			Op:         le.Function,
			Category:   mintCategory(le.Name),
			Source:     SourceMinioMint,
			SDK:        le.Name,
			DurationMs: le.Duration,
		}
		switch strings.ToUpper(strings.TrimSpace(le.Status)) {
		case "PASS":
			e.Status = OpPassed
		case "FAIL":
			e.Status = OpFailed
			e.Detail = compactDetail(le.Alert, le.Message, le.Error)
		case "NA":
			e.Status = OpUnsupported
			e.Detail = compactDetail(le.Alert, le.Message, "")
		default:
			e.Status = OpErrored
			e.Detail = fmt.Sprintf("unknown mint status %q", le.Status)
		}
		out = append(out, e)
	}
	return out, nil
}

// ParseMintLogDir walks a mint-logs/{date}/ directory and parses
// every per-SDK log.json beneath it. mint writes two flavours of
// log file when it runs:
//
//   - {root}/log.json — an aggregated copy that mint.sh produces
//     by `cat`-ing every per-SDK log into a single file at the
//     top of the bind-mount target.
//   - {root}/{sdk}/log.json — the per-SDK source files.
//
// Parsing both would double-count every entry, so we only parse
// files at exactly one subdirectory deep — i.e. {root}/{sdk}/log.json.
// The top-level aggregated log is intentionally skipped because
// the per-SDK files are the authoritative source (the aggregated
// log is just a redundant view, and skipping it makes the entry
// count match an operator's intuition of "one row per real test").
//
// Directories without a log.json (e.g. an empty SDK folder produced
// by a partial run) are silently skipped — the caller can check
// the returned entry count if they expect a specific number of SDKs.
func ParseMintLogDir(root string) ([]MatrixEntry, error) {
	var out []MatrixEntry
	cleanRoot := filepath.Clean(root)
	err := filepath.WalkDir(cleanRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "log.json" {
			return nil
		}
		// Skip the aggregated top-level log.json: its parent
		// directory IS the walk root, so it would duplicate every
		// per-SDK entry.
		if filepath.Dir(path) == cleanRoot {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		entries, err := ParseMintLog(f)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, entries...)
		return nil
	})
	return out, err
}

// Aggregate combines the supplied entry slices into a single
// Matrix stamped with the provided gateway metadata.
func Aggregate(gatewayEndpoint, gatewaySHA string, generatedAt time.Time, parts ...[]MatrixEntry) Matrix {
	var all []MatrixEntry
	for _, p := range parts {
		all = append(all, p...)
	}
	return Matrix{
		GatewayEndpoint: gatewayEndpoint,
		GatewaySHA:      gatewaySHA,
		GeneratedAt:     generatedAt.UTC(),
		Entries:         all,
	}
}

// shortCategory shortens an s3-tests classname like
// "s3tests.functional.test_s3" to just "test_s3".
func shortCategory(classname string) string {
	if i := strings.LastIndex(classname, "."); i >= 0 {
		return classname[i+1:]
	}
	if classname == "" {
		return "external"
	}
	return classname
}

// mintCategory maps an SDK name to the matrix category. mint
// groups by SDK at the filesystem level; the in-process matrix
// uses high-level S3 surface buckets ("core", "multipart", ...)
// but mint doesn't expose that, so we fall back to "external"
// for the parent category and let SDK carry the per-row
// attribution.
func mintCategory(sdk string) string {
	if sdk == "" {
		return "external"
	}
	return sdk
}

// compactDetail collapses up to three optional fragments into a
// single short line suitable for the matrix Detail column. Long
// stack traces are truncated to 240 bytes so the rendered
// matrix stays readable. Truncation is rune-aware: if the byte
// slice [:237] would land in the middle of a multi-byte UTF-8
// sequence (possible when a harness emits a non-ASCII error
// message — e.g. a localised exception text or a file path with
// Unicode), we back off to the previous rune boundary so the
// truncated string remains valid UTF-8 and renders cleanly in
// the JSON matrix.
func compactDetail(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	if len(nonEmpty) == 0 {
		return ""
	}
	joined := strings.Join(nonEmpty, "; ")
	if len(joined) > 240 {
		cut := 237
		for cut > 0 && !utf8.RuneStart(joined[cut]) {
			cut--
		}
		joined = joined[:cut] + "..."
	}
	// Collapse newlines for single-line matrix cells.
	joined = strings.ReplaceAll(joined, "\n", " ")
	joined = strings.ReplaceAll(joined, "\r", " ")
	return joined
}
