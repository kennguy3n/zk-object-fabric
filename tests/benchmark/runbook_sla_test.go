package benchmark

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// runbookSLAPath is the operator-facing runbook whose headline "SLA
// gate" table is the published source of truth for the Target*
// constants in suite.go. The staging Tier 3 run
// (deploy/staging/load-driver/scripts/run_tier3.sh) and the CI smoke
// run both gate on those constants, so the doc and the code must not
// drift apart.
const runbookSLAPath = "../../docs/runbooks/load-testing.md"

// targetConstantsByName maps the constant name as written in the
// runbook table's "Constant in code" column to the live value of that
// Go constant. These are exactly the rows in the headline SLA-gate
// table. Adding a new row to the runbook table without adding it here
// (or vice versa) is caught by TestTargetConstants_MatchRunbook. The
// two ratio gates (TargetCacheHitRatioHotMin,
// TargetWasabiOriginEgressRatioMax) are documented in the runbook
// prose rather than the headline table, so they are pinned separately
// in TestRatioConstants_Documented.
var targetConstantsByName = map[string]float64{
	"TargetPutP99CacheHitMs": TargetPutP99CacheHitMs,
	"TargetPutP99OriginMs":   TargetPutP99OriginMs,
	"TargetGetP99L0Ms":       TargetGetP99L0Ms,
	"TargetGetP99L1Ms":       TargetGetP99L1Ms,
	"TargetGetP99OriginMs":   TargetGetP99OriginMs,
	"TargetSustainedRPS":     TargetSustainedRPS,
	"TargetErrorRateMax":     TargetErrorRateMax,
	"TargetRPSEfficiencyMin": TargetRPSEfficiencyMin,
}

// TestTargetConstants_MatchRunbook parses the SLA-gate table in
// docs/runbooks/load-testing.md and asserts that every documented
// threshold equals the value of the Go constant named in that row.
//
// This ties the published SLA contract to the code: changing a
// constant in suite.go without updating the runbook (or editing the
// runbook number without updating the constant) fails this test. If
// you intentionally change an SLA, change BOTH in the same commit.
func TestTargetConstants_MatchRunbook(t *testing.T) {
	abs, err := filepath.Abs(runbookSLAPath)
	if err != nil {
		t.Fatalf("resolve runbook path: %v", err)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read runbook %s: %v", abs, err)
	}

	rows := parseRunbookSLATable(string(raw))
	if len(rows) == 0 {
		t.Fatalf("no SLA-gate rows parsed from %s; has the table format changed?", runbookSLAPath)
	}

	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		seen[r.constant] = true
		got, ok := targetConstantsByName[r.constant]
		if !ok {
			t.Errorf("runbook references constant %q that this test does not know about; "+
				"add it to targetConstantsByName", r.constant)
			continue
		}
		if got != r.want {
			t.Errorf("%s = %v, runbook documents %v; update docs/runbooks/load-testing.md "+
				"and suite.go together", r.constant, got, r.want)
		}
	}

	// Every constant this test tracks must appear in the runbook so a
	// new gate cannot be added to code while the published contract
	// goes stale.
	for name := range targetConstantsByName {
		if !seen[name] {
			t.Errorf("constant %q is tracked by the drift test but absent from the runbook "+
				"SLA table; document it in docs/runbooks/load-testing.md", name)
		}
	}
}

// TestRatioConstants_Documented pins the two ratio SLA gates that the
// runbook documents in prose (§"SLA gate" narrative and §3) rather
// than in the headline table. They are part of the published contract
// and gated by DefaultSuite's cache-hit-ratio-hot and
// wasabi-origin-egress-ratio scenarios, so a silent change to either
// constant should fail a test.
func TestRatioConstants_Documented(t *testing.T) {
	if TargetCacheHitRatioHotMin != 0.9 {
		t.Errorf("TargetCacheHitRatioHotMin = %v, runbook documents > 0.9", TargetCacheHitRatioHotMin)
	}
	if TargetWasabiOriginEgressRatioMax != 1.0 {
		t.Errorf("TargetWasabiOriginEgressRatioMax = %v, runbook documents <= 1.0", TargetWasabiOriginEgressRatioMax)
	}
}

type runbookSLARow struct {
	constant string
	want     float64
}

// parseRunbookSLATable extracts (constant, threshold) pairs from the
// markdown SLA table. It accepts any table row whose second cell
// contains a single backtick-wrapped Target* identifier and whose
// third cell contains a parseable threshold (e.g. "≤ 50 ms",
// "≥ 10 000 req/s", "≤ 1e-3"). Non-table lines and the header /
// separator rows are ignored.
func parseRunbookSLATable(md string) []runbookSLARow {
	var rows []runbookSLARow
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) < 3 {
			continue
		}
		constant := extractBacktickIdent(cells[1])
		if !strings.HasPrefix(constant, "Target") {
			continue
		}
		val, ok := parseThreshold(cells[2])
		if !ok {
			continue
		}
		rows = append(rows, runbookSLARow{constant: constant, want: val})
	}
	return rows
}

// splitMarkdownRow splits a "| a | b | c |" row into trimmed cells,
// dropping the empty leading/trailing fields produced by the border
// pipes.
func splitMarkdownRow(line string) []string {
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	// Drop the empty fields before the first and after the last pipe.
	if len(out) > 0 && out[0] == "" {
		out = out[1:]
	}
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// extractBacktickIdent returns the identifier inside the first
// pair of backticks in s, or "" if there is none.
func extractBacktickIdent(s string) string {
	start := strings.IndexByte(s, '`')
	if start < 0 {
		return ""
	}
	rest := s[start+1:]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// digitGroupSpaceRe matches a space (regular or non-breaking) sitting
// between two digits, i.e. a thousands separator like "10 000".
var digitGroupSpaceRe = regexp.MustCompile(`(\d)[ \x{00a0}]+(\d)`)

// thresholdNumberRe matches the first numeric literal in a cell,
// including decimals and scientific notation (e.g. "50", "0.95",
// "1e-3").
var thresholdNumberRe = regexp.MustCompile(`[0-9]*\.?[0-9]+(?:[eE][+-]?[0-9]+)?`)

// parseThreshold turns a documented threshold cell into a float. It
// collapses digit-grouping spaces ("10 000" -> "10000") and then
// extracts the first numeric literal, so the comparison operators
// (≤ ≥ <= >=) and unit suffixes (ms, req/s, ratio) are simply ignored
// rather than stripped by substring matching. Returns ok=false when no
// number is present.
func parseThreshold(cell string) (float64, bool) {
	s := cell
	// Collapse thousands-separator spaces until none remain, so a
	// number like "1 000 000" becomes a single contiguous token.
	for digitGroupSpaceRe.MatchString(s) {
		s = digitGroupSpaceRe.ReplaceAllString(s, "$1$2")
	}
	tok := thresholdNumberRe.FindString(s)
	if tok == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(tok, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// TestParseThreshold guards the threshold parser against the cell
// formats used in the runbook SLA table (and a couple of edge cases
// the table does not currently use but could grow into): comparison
// operators, unit suffixes, decimals, scientific notation, regular and
// non-breaking thousands-separator spaces, and non-numeric cells.
func TestParseThreshold(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"≤ 50 ms", 50, true},
		{"≤ 200 ms", 200, true},
		{"≤ 20 ms", 20, true},
		{"≥ 10 000 req/s", 10000, true},
		{"≥ 1\u00a0000\u00a0000 req/s", 1000000, true}, // non-breaking grouping
		{"≤ 1e-3", 1e-3, true},
		{"≥ 0.95", 0.95, true},
		{"<= 0.9 ratio", 0.9, true},
		{"n/a", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		got, ok := parseThreshold(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parseThreshold(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}
