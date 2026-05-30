package dr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration/cross_cell"
	"github.com/kennguy3n/zk-object-fabric/providers"
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
//
// The parent directory of DR_REPORT_FILE is created if it
// doesn't already exist so the CI workflow doesn't have to
// remember to `mkdir -p` before invoking `go test`, and so
// operators running the verifier locally can pass any path
// without a prerequisite step.
func maybeEmitReport(t *testing.T, rpt Report) {
	t.Helper()
	path := os.Getenv("DR_REPORT_FILE")
	if path == "" {
		return
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir parent of dr report %q: %v", path, err)
		}
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

// TestVerifier_BackendMismatchFailsRecovery proves that a
// destination manifest still pointing at the source cell ID
// after the "replication" step is treated as an unrecovered
// object — not silently counted as recovered just because the
// body bytes happen to match. This pins the structural
// guarantee added to verifyRecovery: backend regressions are
// per-object failures, and a run where any piece carries the
// wrong backend cannot return nil.
//
// The test bypasses the replicator and directly seeds the
// destination cell with a manifest whose Backend equals the
// source cell. The body bytes ARE identical to the source body
// (the same deterministic payload), so the body-equality branch
// of verifyRecovery would otherwise pass.
func TestVerifier_BackendMismatchFailsRecovery(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:           src,
		Dest:             dst,
		TenantID:         "t-backend-mismatch",
		Bucket:           "dr",
		SteadyObjects:    1,
		InFlightObjects:  0,
		ObjectSize:       64,
		LagSettleTimeout: time.Second,
	}

	objKey := "steady/obj-000000"
	body := deterministicBody(objKey, v.ObjectSize)
	pieceID := "piece-steady-000000"
	ctx := context.Background()

	// Stage the body in the destination provider so GetPiece
	// returns matching bytes — this is the case Finding 0001
	// flagged as silently counted as recovered.
	if _, err := dst.Provider.PutPiece(ctx, pieceID, bytes.NewReader(body), providers.PutOptions{
		ContentLength: int64(len(body)),
	}); err != nil {
		t.Fatalf("dst.Provider.PutPiece: %v", err)
	}

	hash := sha256Hex(objKey)
	mkey := manifest_store.ManifestKey{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKeyHash: hash,
		VersionID:     "v1",
	}
	// Backend is intentionally the SOURCE cell ID — the bug
	// scenario we are pinning a guard against.
	if err := dst.Manifests.Put(ctx, mkey, &metadata.ObjectManifest{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKey:     objKey,
		ObjectKeyHash: hash,
		VersionID:     "v1",
		ObjectSize:    int64(len(body)),
		ChunkSize:     int64(len(body)),
		Pieces: []metadata.Piece{{
			PieceID:   pieceID,
			Backend:   src.ID, // <-- the regression
			SizeBytes: int64(len(body)),
		}},
	}); err != nil {
		t.Fatalf("dst.Manifests.Put: %v", err)
	}

	rpt := Report{
		SteadyObjects:     v.SteadyObjects,
		FailureDetectedAt: time.Now(),
	}
	steady := []seededObject{{
		manifestKey: mkey,
		pieceID:     pieceID,
		body:        body,
		objectKey:   objKey,
	}}
	err := v.verifyRecovery(ctx, &rpt, steady, time.Now)
	if err == nil {
		t.Fatalf("expected backend-mismatch failure, got nil; report=%+v", rpt)
	}
	if !strings.Contains(err.Error(), "backend=") {
		t.Errorf("error %q does not mention backend; finding 0001 fix may have regressed", err)
	}
	if rpt.RecoveredObjects != 0 {
		t.Errorf("RecoveredObjects = %d, want 0 (backend mismatch must NOT count as recovered)",
			rpt.RecoveredObjects)
	}
	// Finding 0003: RTO must NOT be populated on failed runs.
	if !rpt.RecoveryReadyAt.IsZero() {
		t.Errorf("RecoveryReadyAt = %v, want zero on failed run", rpt.RecoveryReadyAt)
	}
	if rpt.MeasuredRTO != 0 {
		t.Errorf("MeasuredRTO = %s, want 0 on failed run", rpt.MeasuredRTO)
	}
}

// TestVerifier_MeasureRPOCountsMissingInDest pins the Phase-4
// defence-in-depth probe: MeasuredRPO is computed by actually
// counting missing in-flight manifests in the destination cell,
// not by assuming MeasuredRPO == InFlightObjects.
//
// The test stages two seededObjects — one present in the dest
// manifests (a "leaked" object that the Phase-2 cancellation
// failed to prevent) and one absent (the expected lost object).
// measureRPO must surface lost=1, leaked=1.
func TestVerifier_MeasureRPOCountsMissingInDest(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:   src,
		Dest:     dst,
		TenantID: "t-measure-rpo",
		Bucket:   "dr",
	}
	ctx := context.Background()

	absent := seededObject{manifestKey: manifest_store.ManifestKey{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKeyHash: sha256Hex("absent"),
		VersionID:     "v1",
	}, objectKey: "absent"}

	presentKey := manifest_store.ManifestKey{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKeyHash: sha256Hex("present"),
		VersionID:     "v1",
	}
	if err := dst.Manifests.Put(ctx, presentKey, &metadata.ObjectManifest{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKey:     "present",
		ObjectKeyHash: presentKey.ObjectKeyHash,
		VersionID:     "v1",
	}); err != nil {
		t.Fatalf("dst.Manifests.Put(present): %v", err)
	}
	present := seededObject{manifestKey: presentKey, objectKey: "present"}

	lost, leaked, err := v.measureRPO(ctx, []seededObject{absent, present})
	if err != nil {
		t.Fatalf("measureRPO: %v", err)
	}
	if lost != 1 {
		t.Errorf("lost = %d, want 1", lost)
	}
	if leaked != 1 {
		t.Errorf("leaked = %d, want 1", leaked)
	}
}

// TestVerifier_LeakedInFlightFailsRun proves that if the
// Phase-2 ordering invariant is ever broken (an in-flight
// manifest reaches the destination cell despite the replicator
// being cancelled before the in-flight seed), the verifier
// surfaces it as a run-level error rather than silently
// inflating MeasuredRPO. The test pre-stages the in-flight
// manifest in the destination manifest store before Run()
// executes — simulating the regression where a future refactor
// breaks the cancel-before-seed ordering and a piece slips
// through.
func TestVerifier_LeakedInFlightFailsRun(t *testing.T) {
	src, dst := newCellPair(t)
	v := &Verifier{
		Source:              src,
		Dest:                dst,
		TenantID:            "t-leak",
		Bucket:              "dr",
		SteadyObjects:       2,
		InFlightObjects:     1,
		ObjectSize:          64,
		ReplicationInterval: 5 * time.Millisecond,
		LagSettleTimeout:    2 * time.Second,
	}

	// Pre-stage the in-flight manifest in the destination cell
	// at the exact key seedInFlight will use. The replicator
	// would never have produced this on its own because Phase 2
	// cancels it before the in-flight seed — but a regression
	// that re-orders Phase 2 would land the manifest here. The
	// Phase-4 probe must surface this as a run-level error.
	inFlightKey := "in-flight/obj-000002" // SteadyObjects=2, so first in-flight index is 2
	inFlightHash := sha256Hex(inFlightKey)
	if err := dst.Manifests.Put(context.Background(), manifest_store.ManifestKey{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKeyHash: inFlightHash,
		VersionID:     "v1",
	}, &metadata.ObjectManifest{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKey:     inFlightKey,
		ObjectKeyHash: inFlightHash,
		VersionID:     "v1",
	}); err != nil {
		t.Fatalf("pre-stage leaked in-flight manifest: %v", err)
	}

	_, err := v.Run(context.Background())
	if err == nil {
		t.Fatalf("expected phase-2 invariant breach error, got nil")
	}
	if !strings.Contains(err.Error(), "phase-2 invariant breach") {
		t.Errorf("error %q does not mention phase-2 invariant; the Phase-4 probe may have regressed", err)
	}
}

// TestVerifier_MeasureRPOSurfacesStoreError covers the
// branch where a non-ErrNotFound error from the dest manifest
// store does NOT get silently counted as "lost". A flaky store
// would otherwise inflate MeasuredRPO and mask a real outage.
func TestVerifier_MeasureRPOSurfacesStoreError(t *testing.T) {
	src, dst := newCellPair(t)
	dst.Manifests = &flakyManifestStore{
		ManifestStore: dst.Manifests,
		fail:          errors.New("manifest store: connection refused"),
	}
	v := &Verifier{
		Source:   src,
		Dest:     dst,
		TenantID: "t-flaky",
		Bucket:   "dr",
	}
	candidate := seededObject{manifestKey: manifest_store.ManifestKey{
		TenantID:      v.TenantID,
		Bucket:        v.Bucket,
		ObjectKeyHash: sha256Hex("x"),
		VersionID:     "v1",
	}, objectKey: "x"}
	_, _, err := v.measureRPO(context.Background(), []seededObject{candidate})
	if err == nil {
		t.Fatalf("expected flaky-store error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("error %q does not mention upstream cause", err)
	}
}

// flakyManifestStore wraps a real ManifestStore but forces
// Get to return a synthetic non-ErrNotFound error so the
// Phase-4 probe's "is this ErrNotFound?" branch can be tested
// without standing up a broken provider.
type flakyManifestStore struct {
	manifest_store.ManifestStore
	fail error
}

func (f *flakyManifestStore) Get(ctx context.Context, key manifest_store.ManifestKey) (*metadata.ObjectManifest, error) {
	return nil, f.fail
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
