package capacity

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// expectedDossierConstants returns the canonical list of capacity.*
// names that the dossier (docs/CAPACITY.md) must reference. It is the
// single source of truth shared by both directions of the drift
// detector:
//
//   - TestDossierDoc_CitesEveryConstant walks expected -> doc and
//     fails if a name in the list is missing from the doc.
//   - TestDossierDoc_NoUnknownConstantReferences walks doc -> expected
//     and fails if the doc references a capacity.* name that is NOT
//     in the list (catches "stale name in doc after a rename" and
//     "doc references a constant that doesn't actually exist").
//
// When you add a new constant in targets.go, add it to this list AND
// reference it in docs/CAPACITY.md in the same PR. Both tests will
// fail until both moves land together.
func expectedDossierConstants() []string {
	return []string{
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
}

// TestDossierDoc_CitesEveryConstant is the structural drift detector
// between this package and docs/CAPACITY.md. Every constant the
// expectedDossierConstants() list names must appear in the doc with
// its `capacity.Name` form so a reader can jump from the value to
// the source of truth in one grep.
//
// The detector is intentionally name-based, not value-based: the
// per-constant value assertions live in targets_test.go and would
// already catch a literal-value drift. This test catches "constant
// renamed in code, doc never updated" and "constant added in code,
// doc not updated" — i.e. structural drift the value pinning can't
// catch.
func TestDossierDoc_CitesEveryConstant(t *testing.T) {
	doc := readDossierDoc(t)
	var missing []string
	for _, name := range expectedDossierConstants() {
		if !strings.Contains(doc, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("docs/CAPACITY.md is missing references to %d constants — every dossier constant must be cited in the doc by its capacity.Name form so an auditor can grep from value to source-of-truth:\n  %s", len(missing), strings.Join(missing, "\n  "))
	}
}

// TestDossierDoc_NoUnknownConstantReferences is the reverse of
// TestDossierDoc_CitesEveryConstant: every `capacity.Name` mention in
// the doc must correspond to a real constant in
// expectedDossierConstants(). Catches "constant renamed in code,
// stale name still in doc" and "doc references a constant that was
// never declared".
//
// The detector regex matches `capacity.<UpperCamelCase>` so it picks
// up the dossier table entries and any prose mention, but does not
// match prose like "the capacity. of a tenant is..." (no UpperCamel
// follow-up) or unrelated dotted notation.
func TestDossierDoc_NoUnknownConstantReferences(t *testing.T) {
	doc := readDossierDoc(t)
	re := regexp.MustCompile(`capacity\.[A-Z][A-Za-z0-9]*`)
	matches := re.FindAllString(doc, -1)

	known := make(map[string]struct{}, len(expectedDossierConstants()))
	for _, name := range expectedDossierConstants() {
		known[name] = struct{}{}
	}

	unknownSet := make(map[string]struct{})
	for _, m := range matches {
		if _, ok := known[m]; !ok {
			unknownSet[m] = struct{}{}
		}
	}
	if len(unknownSet) > 0 {
		unknown := make([]string, 0, len(unknownSet))
		for name := range unknownSet {
			unknown = append(unknown, name)
		}
		sort.Strings(unknown)
		t.Fatalf("docs/CAPACITY.md references %d capacity.* names that are NOT in expectedDossierConstants() — either the constant was renamed and the doc was not updated, or the doc cites a constant that does not exist:\n  %s", len(unknown), strings.Join(unknown, "\n  "))
	}
}

// TestDossierDoc_ForbidsTheoreticalDurabilityNines pins the
// docs/PROPOSAL.md non-goal: the capacity dossier must NOT publish a
// theoretical durability number. The chaos report is the only
// surface where durability appears, and only as a measured value
// against a specific build.
//
// Each forbidden string has a per-string licit count. Most of them
// have zero licit occurrences — they are anti-patterns that have no
// business appearing in the dossier at all. "eleven nines" has
// exactly one licit occurrence because the docs/PROPOSAL.md §11.4
// quote in §1 of the dossier ("Publish theoretical 'eleven nines'
// durability — Cannot be validated by analysis.") uses it inside the
// quote block to state the non-goal. A second occurrence anywhere
// would be a real claim creeping in.
//
// Allowing one occurrence universally (as the pre-fix shape did)
// would let a claim like "99.999999999%" slip in once, because the
// quote contains "eleven nines" but does NOT contain
// "99.999999999". Per-string licit counts close that hole.
func TestDossierDoc_ForbidsTheoreticalDurabilityNines(t *testing.T) {
	doc := readDossierDoc(t)
	if !strings.Contains(doc, "Cannot be validated by analysis") {
		t.Fatalf("docs/CAPACITY.md missing the docs/PROPOSAL.md §11.4 non-goal quote about theoretical durability — the dossier must explicitly state this is out of scope")
	}

	// licit is the maximum number of times each forbidden string
	// may appear in the doc. Only "eleven nines" has a non-zero
	// licit count because that is the only string actually present
	// in the docs/PROPOSAL.md quote. Adding a new forbidden string
	// with a non-zero licit count requires updating both this map
	// AND the quote text.
	licit := map[string]int{
		"eleven nines":  1, // inside the PROPOSAL.md §11.4 quote
		"11 nines":      0,
		"99.999999999":  0,
		"twelve nines":  0,
		"12 nines":      0,
	}

	lower := strings.ToLower(doc)
	for f, maxAllowed := range licit {
		got := strings.Count(lower, f)
		if got > maxAllowed {
			t.Errorf("docs/CAPACITY.md mentions %q %d times — limit is %d (%s)",
				f, got, maxAllowed, licitRationale(maxAllowed))
		}
	}
}

func licitRationale(maxAllowed int) string {
	if maxAllowed == 0 {
		return "no licit occurrence — this string has no business appearing in the dossier"
	}
	return "the licit occurrences live inside the docs/PROPOSAL.md §11.4 quote in §1"
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
