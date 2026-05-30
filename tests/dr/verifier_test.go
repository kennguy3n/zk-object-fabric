package dr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration/cross_cell"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// maybeEmitReport writes rpt as pretty-printed JSON to the path
// in $DR_REPORT_FILE, if that env var is set. This is the seam
// the CI workflow (.github/workflows/dr-verify.yml) uses to
// collect the verifier output as a build artifact for trend
// analysis. Local test runs ignore the env var entirely; the
// path is treated as fatal if the env var is set but the write
// fails so a misconfigured CI run cannot silently lose the
// report (a missing artifact would otherwise look identical to
// "test never ran").
func maybeEmitReport(t *testing.T, rpt Report) {
	t.Helper()
	path := os.Getenv("DR_REPORT_FILE")
	if path == "" {
		return
	}
	buf, err := json.MarshalIndent(rpt, "", "  ")
	if err != nil {
		t.Fatalf("marshal dr report: %v", err)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatalf("write dr report to %q: %v", path, err)
	}
}

// newCellPair returns a Source / Dest cell pair backed by
// separate in-memory manifest stores and separate local_fs_dev
// providers. Each cell's provider is rooted in its own t.TempDir
// subdirectory so the underlying filesystem cannot accidentally
// hide a replicator regression (e.g. by sharing inodes).
func newCellPair(t *testing.T) (cross_cell.Cell, cross_cell.Cell) {
	t.Helper()
	root := t.TempDir()
	srcProv, err := local_fs_dev.New(filepath.Join(root, "src"))
	if err != nil {
		t.Fatalf("local_fs_dev.New(src): %v", err)
	}
	dstProv, err := local_fs_dev.New(filepath.Join(root, "dst"))
	if err != nil {
		t.Fatalf("local_fs_dev.New(dst): %v", err)
	}
	return cross_cell.Cell{
			ID:        "src-cell",
			Manifests: memory.New(),
			Provider:  srcProv,
		}, cross_cell.Cell{
			ID:        "dst-cell",
			Manifests: memory.New(),
			Provider:  dstProv,
		}
}

// TestVerifier_HappyPath drives the verifier end-to-end against
// fresh in-memory cells and asserts every report invariant:
//
//   - All SteadyObjects are recoverable byte-for-byte from the
//     destination cell.
//   - LostObjects equals InFlightObjects exactly (the RPO
//     contract — Phase 2 cancels the replicator before seeding
//     in-flight precisely so this is deterministic).
//   - MeasuredRTO is non-negative and bounded by RTOTarget.
//   - ReplicatorPiecesCopied >= SteadyObjects (the replicator
//     copied at least one piece per steady object).
func TestVerifier_HappyPath(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:              src,
		Dest:                dst,
		TenantID:            "t-dr",
		Bucket:              "dr-happy",
		SteadyObjects:       12,
		InFlightObjects:     3,
		ObjectSize:          512,
		ReplicationInterval: 5 * time.Millisecond,
		LagSettleTimeout:    2 * time.Second,
		RTOTarget:           5 * time.Second,
	}
	rpt, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Verifier.Run: %v\nreport=%+v", err, rpt)
	}
	if rpt.RecoveredObjects != v.SteadyObjects {
		t.Errorf("RecoveredObjects = %d, want %d", rpt.RecoveredObjects, v.SteadyObjects)
	}
	if rpt.LostObjects != v.InFlightObjects {
		t.Errorf("LostObjects = %d, want %d", rpt.LostObjects, v.InFlightObjects)
	}
	if rpt.MeasuredRPO != v.InFlightObjects {
		t.Errorf("MeasuredRPO = %d, want %d", rpt.MeasuredRPO, v.InFlightObjects)
	}
	if rpt.MeasuredRTO <= 0 {
		t.Errorf("MeasuredRTO = %s, want > 0", rpt.MeasuredRTO)
	}
	if rpt.MeasuredRTO > v.RTOTarget {
		t.Errorf("MeasuredRTO = %s exceeds target %s", rpt.MeasuredRTO, v.RTOTarget)
	}
	if rpt.ReplicatorPiecesCopied < int64(v.SteadyObjects) {
		t.Errorf("ReplicatorPiecesCopied = %d, want >= %d",
			rpt.ReplicatorPiecesCopied, v.SteadyObjects)
	}
	if rpt.FailureDetectedAt.IsZero() {
		t.Error("FailureDetectedAt is zero")
	}
	if rpt.RecoveryReadyAt.IsZero() {
		t.Error("RecoveryReadyAt is zero")
	}
	maybeEmitReport(t, rpt)
}

// TestVerifier_NoInFlight pins the case where the operator
// catches the failure before any post-snapshot writes happen.
// RPO must be zero and the dest cell must hold every steady
// object.
func TestVerifier_NoInFlight(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:              src,
		Dest:                dst,
		TenantID:            "t-dr",
		Bucket:              "dr-no-inflight",
		SteadyObjects:       5,
		InFlightObjects:     0,
		ObjectSize:          256,
		ReplicationInterval: 5 * time.Millisecond,
		LagSettleTimeout:    2 * time.Second,
		RTOTarget:           5 * time.Second,
	}
	rpt, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Verifier.Run: %v\nreport=%+v", err, rpt)
	}
	if rpt.LostObjects != 0 || rpt.MeasuredRPO != 0 {
		t.Errorf("zero in-flight: LostObjects=%d MeasuredRPO=%d, want 0/0",
			rpt.LostObjects, rpt.MeasuredRPO)
	}
}

// TestVerifier_RTOBreach trips the RTO target by setting a 1ns
// target. The verifier must (a) still complete recovery (so the
// report is populated), (b) return an RTO breach error.
func TestVerifier_RTOBreach(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:              src,
		Dest:                dst,
		TenantID:            "t-dr",
		Bucket:              "dr-rto-breach",
		SteadyObjects:       2,
		InFlightObjects:     0,
		ObjectSize:          128,
		ReplicationInterval: 5 * time.Millisecond,
		LagSettleTimeout:    2 * time.Second,
		RTOTarget:           1, // 1 nanosecond is unattainable
	}
	rpt, err := v.Run(context.Background())
	if err == nil {
		t.Fatalf("expected RTO breach error, got nil; report=%+v", rpt)
	}
	if rpt.RecoveredObjects != v.SteadyObjects {
		t.Errorf("RecoveredObjects = %d, want %d (recovery should still complete)",
			rpt.RecoveredObjects, v.SteadyObjects)
	}
}

// TestVerifier_ValidationErrors covers the configuration
// guards. Each subtest mutates a single field and asserts Run
// fails before any I/O. The test is cheap and keeps the
// configuration contract visible to readers.
func TestVerifier_ValidationErrors(t *testing.T) {
	src, dst := newCellPair(t)
	base := func() *Verifier {
		return &Verifier{
			Source:           src,
			Dest:             dst,
			TenantID:         "t",
			Bucket:           "b",
			SteadyObjects:    1,
			InFlightObjects:  0,
			ObjectSize:       64,
			LagSettleTimeout: time.Second,
		}
	}
	cases := []struct {
		name   string
		mutate func(v *Verifier)
	}{
		{"empty TenantID", func(v *Verifier) { v.TenantID = "" }},
		{"empty Bucket", func(v *Verifier) { v.Bucket = "" }},
		{"zero SteadyObjects", func(v *Verifier) { v.SteadyObjects = 0 }},
		{"negative InFlightObjects", func(v *Verifier) { v.InFlightObjects = -1 }},
		{"zero ObjectSize", func(v *Verifier) { v.ObjectSize = 0 }},
		{"zero LagSettleTimeout", func(v *Verifier) { v.LagSettleTimeout = 0 }},
		{"same cell IDs", func(v *Verifier) { v.Dest.ID = v.Source.ID }},
		{"missing Source.Manifests", func(v *Verifier) { v.Source.Manifests = nil }},
		{"missing Dest.Provider", func(v *Verifier) { v.Dest.Provider = nil }},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			v := base()
			tc.mutate(v)
			rpt, err := v.Run(context.Background())
			if err == nil {
				t.Fatalf("expected validation error, got nil; report=%+v", rpt)
			}
		})
	}
}

// TestVerifier_PreCancelledContext asserts the verifier
// surfaces ctx cancellation rather than silently succeeding.
// A pre-cancelled context must fail the run before any I/O
// produces side-effects on the cells.
func TestVerifier_PreCancelledContext(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:              src,
		Dest:                dst,
		TenantID:            "t",
		Bucket:              "b",
		SteadyObjects:       10,
		InFlightObjects:     0,
		ObjectSize:          128,
		ReplicationInterval: 5 * time.Millisecond,
		LagSettleTimeout:    2 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := v.Run(ctx)
	if err == nil {
		t.Fatalf("expected cancellation error, got nil")
	}
}
