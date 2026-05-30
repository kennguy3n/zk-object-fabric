package s3_conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// OpStatus is the typed outcome of a single conformance operation.
type OpStatus string

const (
	// OpPassed means the server returned the response the AWS S3
	// reference behaviour requires.
	OpPassed OpStatus = "passed"

	// OpFailed means the server returned a response, but the response
	// did not match the AWS S3 reference behaviour. The .Detail field
	// describes the divergence (e.g. "ETag mismatch", "wrong status
	// 200, expected 416", "ListObjects returned 0 keys, expected 3").
	// Failed is a real defect to triage.
	OpFailed OpStatus = "failed"

	// OpUnsupported means the operation is not implemented by the
	// gateway today and the server returned a 4xx (typically 501 Not
	// Implemented, 400 Bad Request, or 403 Forbidden when the
	// authoriser shadow-rejects the request). The .Detail field
	// carries the HTTP status + body so the matrix consumer can tell
	// "we know we don't support this" apart from "we accidentally
	// 500'd". Unsupported is a documented gap, not a defect.
	OpUnsupported OpStatus = "unsupported"

	// OpErrored means the runner could not get a meaningful response
	// from the server (network error, panic, unexpected 5xx). This is
	// distinct from OpFailed — Errored points at infrastructure or
	// the runner itself; Failed points at the gateway's behaviour.
	OpErrored OpStatus = "errored"
)

// OpResult captures the outcome of a single conformance operation.
//
// Op is the canonical AWS SDK operation name (e.g. "PutObject",
// "ListObjectsV2"). Category groups related operations in the
// rendered matrix.
type OpResult struct {
	Op       string        `json:"op"`
	Category string        `json:"category"`
	Status   OpStatus      `json:"status"`
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration_ns"`
}

// Matrix is the full conformance report for a single endpoint run.
//
// The slice is deterministically ordered (sorted by Category then Op)
// so two runs with the same outcome serialise identically — this is
// important because the Matrix is committed to the repo as a
// snapshot and reviewers diff against it.
type Matrix struct {
	Generated  time.Time  `json:"generated"`
	Endpoint   string     `json:"endpoint"`
	Bucket     string     `json:"bucket"`
	Operations []OpResult `json:"operations"`
}

// Sort returns m with Operations sorted by (Category, Op). It is
// idempotent and safe to call multiple times.
func (m *Matrix) Sort() {
	sort.SliceStable(m.Operations, func(i, j int) bool {
		if m.Operations[i].Category != m.Operations[j].Category {
			return m.Operations[i].Category < m.Operations[j].Category
		}
		return m.Operations[i].Op < m.Operations[j].Op
	})
}

// AllPassed reports whether every result is OpPassed OR OpUnsupported
// (unsupported is a documented gap, not a regression). It returns
// false the moment any OpFailed or OpErrored is present.
//
// This is the CI gate: a passing run must have zero Failed/Errored
// entries. Unsupported entries are expected; their count is published
// in the matrix as the surface area gap, not as a test failure.
func (m *Matrix) AllPassed() bool {
	for _, r := range m.Operations {
		switch r.Status {
		case OpFailed, OpErrored:
			return false
		}
	}
	return true
}

// Counts returns the count of each status in the matrix. The returned
// map always has all four status keys present (with zero values for
// missing ones) so callers can render a compact summary table without
// nil-key handling.
func (m *Matrix) Counts() map[OpStatus]int {
	out := map[OpStatus]int{
		OpPassed:      0,
		OpFailed:      0,
		OpUnsupported: 0,
		OpErrored:     0,
	}
	for _, r := range m.Operations {
		out[r.Status]++
	}
	return out
}

// WriteJSON renders the matrix as pretty-printed JSON. The output is
// deterministic for a given matrix (Sort is called as a side effect).
func (m *Matrix) WriteJSON(w io.Writer) error {
	m.Sort()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// WriteMarkdown renders the matrix as a Markdown document suitable
// for committing under docs/conformance/. The output is deterministic
// for a given matrix (Sort is called as a side effect).
//
// The layout mirrors the AWS S3 API reference grouping: one section
// per Category, a 4-column table per section (Op, Status, Detail,
// Duration), and a summary line at the top with the per-status counts.
func (m *Matrix) WriteMarkdown(w io.Writer) error {
	m.Sort()
	counts := m.Counts()

	// errWriter holds the first error returned by any fmt.Fprintf
	// call and short-circuits subsequent writes once it is non-nil.
	// We use this instead of inline `if _, err := ...; err != nil`
	// boilerplate at every line so the rendering logic stays
	// readable while still propagating disk-full / broken-pipe
	// failures up to the caller — symmetric with WriteJSON's
	// encoder error propagation. The final ew.err is returned at
	// the bottom of the function.
	ew := &errWriter{w: w}

	ew.printf("# S3 Conformance Matrix\n\n")
	ew.printf("| Field | Value |\n|---|---|\n")
	ew.printf("| Generated | %s |\n", m.Generated.UTC().Format(time.RFC3339))
	ew.printf("| Endpoint | %s |\n", escapeMD(m.Endpoint))
	ew.printf("| Bucket | %s |\n", escapeMD(m.Bucket))
	ew.printf("| Passed | %d |\n", counts[OpPassed])
	ew.printf("| Failed | %d |\n", counts[OpFailed])
	ew.printf("| Unsupported | %d |\n", counts[OpUnsupported])
	ew.printf("| Errored | %d |\n", counts[OpErrored])
	ew.println()

	if counts[OpFailed] > 0 || counts[OpErrored] > 0 {
		ew.println("> **Gate status: REGRESSION.** At least one operation is")
		ew.println("> Failed or Errored. Triage these before merging.")
		ew.println()
	} else {
		ew.println("> **Gate status: PASS.** Every implemented operation")
		ew.println("> returned the expected AWS S3 reference behaviour;")
		ew.println("> Unsupported entries are the documented gateway gaps.")
		ew.println()
	}

	// Emit one section per category. A blank line is printed
	// before every new heading EXCEPT the first one — strict
	// CommonMark parsers (e.g. pandoc with --strict, some
	// static-site generators) require a blank line between a
	// table's last row and the following block element. GitHub
	// Flavored Markdown is lenient and would render correctly
	// either way, but the matrix is also published as a CI
	// artifact that may be ingested by other tooling, so the
	// portable form is the safer default.
	cat := ""
	first := true
	for _, r := range m.Operations {
		if r.Category != cat {
			cat = r.Category
			if !first {
				ew.println()
			}
			first = false
			ew.printf("## %s\n\n", categoryTitle(cat))
			ew.println("| Operation | Status | Detail | Duration |")
			ew.println("|---|---|---|---|")
		}
		ew.printf("| `%s` | %s | %s | %s |\n",
			r.Op, statusBadge(r.Status), escapeMD(r.Detail), r.Duration.Round(time.Microsecond))
	}
	ew.println()
	return ew.err
}

// errWriter is a tiny sticky-error wrapper around io.Writer used by
// WriteMarkdown so we don't have to thread an error check through
// every formatted line of the report. Once err is non-nil, all
// subsequent printf / println calls become no-ops; the final err is
// returned to the caller.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...interface{}) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

func (e *errWriter) println(args ...interface{}) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintln(e.w, args...)
}

// categoryTitle renders an internal category id (e.g. "core") as a
// human-readable section title ("Core operations").
func categoryTitle(cat string) string {
	switch cat {
	case "core":
		return "Core object operations"
	case "bucket":
		return "Bucket operations"
	case "listing":
		return "Listing operations"
	case "range":
		return "Range and conditional GET"
	case "multipart":
		return "Multipart upload"
	case "copy":
		return "Copy"
	case "versioning":
		return "Object versioning"
	case "acl":
		return "Access-control lists (intentionally unsupported)"
	case "tagging":
		return "Object tagging (intentionally unsupported)"
	case "lifecycle":
		return "Bucket lifecycle (intentionally unsupported)"
	case "bucket-versioning":
		return "Bucket versioning toggle (intentionally unsupported)"
	case "bulk":
		return "Bulk operations"
	}
	return cat
}

// statusBadge renders an OpStatus as a Markdown-friendly badge that
// reads well in both rendered Markdown and raw text. We intentionally
// avoid emoji so the matrix is greppable and diffs cleanly.
func statusBadge(s OpStatus) string {
	switch s {
	case OpPassed:
		return "**PASS**"
	case OpFailed:
		return "**FAIL**"
	case OpUnsupported:
		return "_unsupported_"
	case OpErrored:
		return "**ERROR**"
	}
	return string(s)
}

// escapeMD escapes characters that have meaning in a Markdown table
// cell. Specifically: `|` would break the table; `\` is the escape
// character; newlines are flattened to "  " so multi-line detail
// strings render on one row.
func escapeMD(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	return s
}
