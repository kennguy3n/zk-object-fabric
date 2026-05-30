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

	fmt.Fprintf(w, "# S3 Conformance Matrix\n\n")
	fmt.Fprintf(w, "| Field | Value |\n|---|---|\n")
	fmt.Fprintf(w, "| Generated | %s |\n", m.Generated.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "| Endpoint | %s |\n", escapeMD(m.Endpoint))
	fmt.Fprintf(w, "| Bucket | %s |\n", escapeMD(m.Bucket))
	fmt.Fprintf(w, "| Passed | %d |\n", counts[OpPassed])
	fmt.Fprintf(w, "| Failed | %d |\n", counts[OpFailed])
	fmt.Fprintf(w, "| Unsupported | %d |\n", counts[OpUnsupported])
	fmt.Fprintf(w, "| Errored | %d |\n", counts[OpErrored])
	fmt.Fprintln(w)

	if counts[OpFailed] > 0 || counts[OpErrored] > 0 {
		fmt.Fprintln(w, "> **Gate status: REGRESSION.** At least one operation is")
		fmt.Fprintln(w, "> Failed or Errored. Triage these before merging.")
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "> **Gate status: PASS.** Every implemented operation")
		fmt.Fprintln(w, "> returned the expected AWS S3 reference behaviour;")
		fmt.Fprintln(w, "> Unsupported entries are the documented gateway gaps.")
		fmt.Fprintln(w)
	}

	cat := ""
	for _, r := range m.Operations {
		if r.Category != cat {
			cat = r.Category
			fmt.Fprintf(w, "## %s\n\n", categoryTitle(cat))
			fmt.Fprintln(w, "| Operation | Status | Detail | Duration |")
			fmt.Fprintln(w, "|---|---|---|---|")
		}
		fmt.Fprintf(w, "| `%s` | %s | %s | %s |\n",
			r.Op, statusBadge(r.Status), escapeMD(r.Detail), r.Duration.Round(time.Microsecond))
	}
	fmt.Fprintln(w)
	return nil
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
