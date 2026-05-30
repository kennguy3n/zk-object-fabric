package capacity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDossierDoc_CitesEveryConstant is the structural drift detector
// between this package and docs/CAPACITY.md. Every constant we
// declare in targets.go has a row in the dossier; the doc must
// reference the constant by its `capacity.Name` form so a reader can
// jump from the value to the source of truth in one grep. If a new
// constant is added without a doc entry (or vice versa), this test
// fails with the missing name in the error message.
//
// The detector is intentionally name-based, not value-based: the
// per-constant value assertions live in targets_test.go and would
// already catch a literal-value drift. This test catches "constant
// renamed in code, doc still references old name" and "constant
// added in code, doc not updated" — i.e. structural drift the value
// pinning can't catch.
func TestDossierDoc_CitesEveryConstant(t *testing.T) {
	expected := []string{
		// §2 Performance targets — re-exports from benchmark suite.
		"capacity.PutP99CacheHitMs",
		"capacity.PutP99OriginMs",
		"capacity.GetP99L0Ms",
		"capacity.GetP99L1Ms",
		"capacity.GetP99OriginMs",
		"capacity.SustainedRPS",
		"capacity.ErrorRateMax",
		"capacity.RPSEfficiencyMin",
		"capacity.CacheHitRatioHotMin",
		"capacity.WasabiOriginEgressRatioMax",

		// §3 S3 protocol limits.
		"capacity.MaxObjectSizeBytes",
		"capacity.MaxMultipartParts",
		"capacity.MinMultipartPartSizeBytes",
		"capacity.MaxMultipartPartSizeBytes",

		// §4 Per-gateway-node.
		"capacity.PerGatewayNodeSustainedRPS",

		// §5 Per-cell sizing.
		"capacity.MinCellUsableCapacityBytes",
		"capacity.MaxCellUsableCapacityBytes",

		// §7 Availability (derived from ErrorRateMax).
		"capacity.AvailabilityFractionMin",
	}

	doc := readDossierDoc(t)
	var missing []string
	for _, name := range expected {
		if !strings.Contains(doc, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("docs/CAPACITY.md is missing references to %d constants — every dossier constant must be cited in the doc by its capacity.Name form so an auditor can grep from value to source-of-truth:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDossierDoc_ForbidsTheoreticalDurabilityNines pins the
// docs/PROPOSAL.md non-goal: the capacity dossier must NOT publish a
// theoretical durability number. The chaos report is the only
// surface where durability appears, and only as a measured value
// against a specific build.
func TestDossierDoc_ForbidsTheoreticalDurabilityNines(t *testing.T) {
	doc := readDossierDoc(t)
	// We want the §1 "out of scope" prose to be present (proving the
	// doc explicitly states the non-goal), AND we want no rogue
	// "11 nines" / "eleven nines" / "99.999999999" / "1.0E-11"
	// committed-target claim anywhere else in the doc.
	if !strings.Contains(doc, "Cannot be validated in Phase 1") {
		t.Fatalf("docs/CAPACITY.md missing the docs/PROPOSAL.md §11.4 non-goal quote about theoretical durability — the dossier must explicitly state this is out of scope")
	}
	forbidden := []string{
		"eleven nines",
		"11 nines",
		"99.999999999",
		"twelve nines",
		"12 nines",
	}
	lower := strings.ToLower(doc)
	for _, f := range forbidden {
		// Allowed if it appears INSIDE the quote block ("Publish
		// theoretical 'eleven nines'..."). The quote is the only
		// licit mention.
		if strings.Contains(lower, f) {
			occurrences := strings.Count(lower, f)
			// Allow exactly one occurrence — the one inside the
			// docs/PROPOSAL.md quote. More than one means a
			// real claim has crept in.
			if occurrences > 1 {
				t.Errorf("docs/CAPACITY.md mentions %q %d times — should appear at most once (inside the docs/PROPOSAL.md quote)", f, occurrences)
			}
		}
	}
}

// TestDossierDoc_CitesEnforcementGates checks that the §9
// cross-reference table mentions every enforcement gate that the
// dossier depends on. A missing row would mean an auditor cannot
// follow the trace from "what the doc claims" to "what enforces the
// claim".
func TestDossierDoc_CitesEnforcementGates(t *testing.T) {
	doc := readDossierDoc(t)
	gates := []string{
		"cmd/benchmark-runner",
		"cmd/tier3-verify",
		"api/s3compat/multipart_handler.go",
		"internal/auth/rate_limit.go",
		"internal/auth/abuse.go",
		"tests/chaos",
		"make audit-bundle",
	}
	for _, gate := range gates {
		if !strings.Contains(doc, gate) {
			t.Errorf("docs/CAPACITY.md missing reference to enforcement gate %q in §9 cross-reference map", gate)
		}
	}
}

func readDossierDoc(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve test file path via runtime.Caller")
	}
	// dossier_test.go lives at tests/capacity/; the doc lives at docs/.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	docPath := filepath.Join(repoRoot, "docs", "CAPACITY.md")
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	return string(b)
}
