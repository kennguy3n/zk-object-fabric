package dr

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/migration/cross_cell"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Verifier drives an end-to-end disaster-recovery cycle and
// produces a Report with measured RPO/RTO. It is not safe for
// concurrent use: each Verifier instance owns the source and
// destination cell pair for the run.
//
// Lifecycle:
//
//	v := &Verifier{Source: src, Dest: dst, ...}
//	rpt, err := v.Run(ctx)
//
// Internally Run() drives four phases (steady, snapshot,
// in-flight, recovery) over the real cross_cell.Replicator. Each
// phase has explicit synchronisation points so the wall-clock
// measurements are deterministic — see the phase comments in
// Run() for the full timeline.
type Verifier struct {
	// Source and Dest are the cells the verifier replicates
	// between. Their ManifestStore and Provider implementations
	// are exercised end-to-end; do not pass mocks unless the
	// implementation under test is the mock itself.
	Source cross_cell.Cell
	Dest   cross_cell.Cell

	// TenantID / Bucket scope every manifest the verifier writes.
	// The runbook-driven harness uses one tenant + one bucket
	// per cycle so cross-tenant interference cannot mask a DR
	// regression.
	TenantID string
	Bucket   string

	// SteadyObjects is the count of objects PUT and drained to
	// the destination cell before the snapshot. These objects
	// must all be recoverable; any miss is a hard test failure.
	SteadyObjects int

	// InFlightObjects is the count of objects PUT after the
	// snapshot but before the simulated failure. These objects
	// constitute the RPO (data lost on failure). The verifier
	// asserts the measured RPO equals InFlightObjects exactly,
	// pinning the replicator's "drain-before-snapshot" guarantee.
	InFlightObjects int

	// ObjectSize is the body size for each PUT. Sizes too small
	// hide buffering bugs in the provider; sizes too large
	// dominate wall-clock with I/O. 1 KiB is a reasonable
	// default for the in-process harness.
	ObjectSize int

	// ReplicationInterval is the cross_cell.Replicator's tick
	// cadence during the steady-state drain. Defaults to 10ms
	// (the verifier polls aggressively so test runs are fast).
	// Production deployments use 30-60s ticks; the longer
	// cadence is tested via the LagSettleTimeout below.
	ReplicationInterval time.Duration

	// LagSettleTimeout bounds how long Run() waits for the
	// replicator to drain SteadyObjects pieces before declaring
	// "the steady phase is settled". A timeout here is a hard
	// failure: the test fixture is supposed to drive replication
	// to completion before the snapshot.
	LagSettleTimeout time.Duration

	// RTOTarget is the operator-supplied upper bound on RTO.
	// Run() returns an error if the measured RTO exceeds this
	// value. A zero target disables the gate (the measurement
	// is still recorded in the Report).
	RTOTarget time.Duration

	// Now overrides time.Now for testing. Leave nil in
	// production runs.
	Now func() time.Time
}

// Report is the verifier's structured output. Every field is
// safe to JSON-encode for periodic snapshots in CI artifacts.
type Report struct {
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      time.Time     `json:"finished_at"`
	SteadyObjects   int           `json:"steady_objects"`
	InFlightObjects int           `json:"in_flight_objects"`

	// RecoveredObjects is the number of distinct steady objects
	// whose bodies the verifier read back from the destination
	// cell after the simulated failure. A successful run has
	// RecoveredObjects == SteadyObjects.
	RecoveredObjects int `json:"recovered_objects"`

	// LostObjects is the number of in-flight objects that did
	// not reach the destination. A successful run has
	// LostObjects == InFlightObjects (we intentionally do not
	// drain them before failure so the harness exercises the
	// realistic case of a mid-PUT outage).
	LostObjects int `json:"lost_objects"`

	// MeasuredRPO is the number of objects lost expressed as
	// "data points beyond the recoverable snapshot". For the
	// in-process harness this is identical to LostObjects.
	MeasuredRPO int `json:"measured_rpo_objects"`

	// FailureDetectedAt is the wall-clock moment the verifier
	// declared the source cell down (i.e. cut off PUTs and
	// stopped the replicator). RTOMeasurement is taken relative
	// to this point.
	FailureDetectedAt time.Time `json:"failure_detected_at"`

	// RecoveryReadyAt is the wall-clock moment the destination
	// cell served its first successful GET after the failure.
	RecoveryReadyAt time.Time `json:"recovery_ready_at"`

	// MeasuredRTO is RecoveryReadyAt - FailureDetectedAt. The
	// verifier returns an error if this exceeds RTOTarget when
	// a target is configured.
	MeasuredRTO time.Duration `json:"measured_rto"`

	// ReplicatorLagAtSnapshot captures Replicator.LagNanos at
	// the snapshot point. A high value here means the steady
	// drain did not actually settle and the test fixture should
	// be adjusted (LagSettleTimeout too tight, ReplicationInterval
	// too long, etc.).
	ReplicatorLagAtSnapshot time.Duration `json:"replicator_lag_at_snapshot"`

	// ReplicatorPiecesCopied is the lifetime CopiedPieces count
	// at the end of the run. A run where the replicator made
	// zero progress would have a zero here; the verifier
	// returns an error in that case.
	ReplicatorPiecesCopied int64 `json:"replicator_pieces_copied"`
}

// Run drives the verifier and returns the Report. The returned
// error is non-nil only when a hard invariant fails: a recovered
// object body does not match the PUT body, RecoveredObjects is
// short of SteadyObjects, the measured RPO does not equal
// InFlightObjects, or the measured RTO exceeds RTOTarget. The
// per-object diagnostics are not in the Report (they would
// blow up the artifact) but are surfaced through the error
// message so a CI failure is actionable without re-running.
func (v *Verifier) Run(ctx context.Context) (Report, error) {
	if v == nil {
		return Report{}, errors.New("dr: nil verifier")
	}
	if err := v.validate(); err != nil {
		return Report{}, err
	}
	now := v.nowFn()
	rpt := Report{
		StartedAt:       now(),
		SteadyObjects:   v.SteadyObjects,
		InFlightObjects: v.InFlightObjects,
	}

	// --------------------------------------------------------
	// Phase 1: Steady state.
	//
	// Write SteadyObjects manifests under (TenantID, Bucket)
	// with an async replication policy targeting (Source -> Dest)
	// and stage matching pieces in the source provider. Then
	// start the replicator and wait for it to drain.
	// --------------------------------------------------------
	repl := cross_cell.NewReplicator(v.Source, v.Dest, []cross_cell.ScopeKey{{
		TenantID: v.TenantID,
		Bucket:   v.Bucket,
	}})
	repl.Interval = v.replicationInterval()

	steadyObjects, err := v.seedSteady(ctx)
	if err != nil {
		return rpt, fmt.Errorf("seed steady: %w", err)
	}

	replCtx, cancelRepl := context.WithCancel(ctx)
	replDone := make(chan error, 1)
	go func() { replDone <- repl.Run(replCtx) }()

	if err := v.waitForDrain(ctx, repl, steadyObjects); err != nil {
		cancelRepl()
		<-replDone
		return rpt, fmt.Errorf("wait for steady drain: %w", err)
	}
	rpt.ReplicatorLagAtSnapshot = time.Duration(repl.LagNanos())

	// --------------------------------------------------------
	// Phase 2: Cancel the replicator FIRST, then seed in-flight.
	//
	// Order matters: cancelling the replicator before the
	// in-flight seed pins LostObjects == InFlightObjects
	// exactly. If we seeded first and cancelled second the
	// replicator's last tick before cancellation could drain
	// some of the in-flight objects, and the RPO assertion
	// (MeasuredRPO == InFlightObjects) would race. The
	// production analogue is "the operator detects the
	// failure and the replicator stops PUTing to the dest
	// cell before more steady-state PUTs land in the source"
	// — which is the case this ordering models.
	// --------------------------------------------------------
	cancelRepl()
	if err := <-replDone; err != nil && !errors.Is(err, context.Canceled) {
		return rpt, fmt.Errorf("replicator returned unexpected error: %w", err)
	}
	rpt.ReplicatorPiecesCopied = repl.CopiedPieces()
	if rpt.ReplicatorPiecesCopied == 0 {
		return rpt, errors.New("replicator made zero progress; fixture is broken")
	}

	inFlightObjects, err := v.seedInFlight(ctx)
	if err != nil {
		return rpt, fmt.Errorf("seed in-flight: %w", err)
	}

	rpt.FailureDetectedAt = now()

	// --------------------------------------------------------
	// Phase 3: Recovery.
	//
	// "Open" the destination cell for reads. We measure RTO as
	// wall-clock from FailureDetectedAt to the moment the first
	// steady-object GET succeeds against the destination cell.
	// --------------------------------------------------------
	if err := v.verifyRecovery(ctx, &rpt, steadyObjects, now); err != nil {
		return rpt, err
	}

	// Phase 4: Measure RPO by actually probing the destination
	// cell for the in-flight manifests. We do NOT assume
	// LostObjects == InFlightObjects by construction — if a
	// future refactor breaks the Phase-2 ordering and lets the
	// replicator drain some in-flight pieces before cancellation,
	// the measurement here surfaces it as a Phase-2 invariant
	// breach rather than silently inflating LostObjects.
	lost, leaked, err := v.measureRPO(ctx, inFlightObjects)
	if err != nil {
		return rpt, fmt.Errorf("measure RPO: %w", err)
	}
	if leaked > 0 {
		return rpt, fmt.Errorf(
			"phase-2 invariant breach: %d in-flight manifests reached the destination cell despite the replicator being cancelled before the in-flight seed; the ordering in Run() is the only guarantee that MeasuredRPO == InFlightObjects, so any leakage here means a future refactor broke it",
			leaked,
		)
	}
	rpt.LostObjects = lost
	rpt.MeasuredRPO = lost
	rpt.FinishedAt = now()

	if v.RTOTarget > 0 && rpt.MeasuredRTO > v.RTOTarget {
		return rpt, fmt.Errorf(
			"RTO breach: measured %s exceeds target %s (recovered %d/%d steady objects)",
			rpt.MeasuredRTO, v.RTOTarget, rpt.RecoveredObjects, v.SteadyObjects,
		)
	}
	return rpt, nil
}

func (v *Verifier) validate() error {
	if v.Source.ID == "" || v.Source.Manifests == nil || v.Source.Provider == nil {
		return errors.New("dr: Verifier.Source must have ID/Manifests/Provider set")
	}
	if v.Dest.ID == "" || v.Dest.Manifests == nil || v.Dest.Provider == nil {
		return errors.New("dr: Verifier.Dest must have ID/Manifests/Provider set")
	}
	if v.Source.ID == v.Dest.ID {
		return errors.New("dr: Verifier.Source.ID and Dest.ID must differ")
	}
	if v.TenantID == "" || v.Bucket == "" {
		return errors.New("dr: TenantID and Bucket are required")
	}
	if v.SteadyObjects <= 0 {
		return errors.New("dr: SteadyObjects must be > 0")
	}
	if v.InFlightObjects < 0 {
		return errors.New("dr: InFlightObjects must be >= 0")
	}
	if v.ObjectSize <= 0 {
		return errors.New("dr: ObjectSize must be > 0")
	}
	if v.LagSettleTimeout <= 0 {
		return errors.New("dr: LagSettleTimeout must be > 0")
	}
	return nil
}

func (v *Verifier) replicationInterval() time.Duration {
	if v.ReplicationInterval > 0 {
		return v.ReplicationInterval
	}
	return 10 * time.Millisecond
}

func (v *Verifier) nowFn() func() time.Time {
	if v.Now != nil {
		return v.Now
	}
	return time.Now
}

// seededObject pairs the manifest's stable key with the body
// the verifier PUT into the source cell, so the recovery phase
// can compare bytes without re-deriving them.
type seededObject struct {
	manifestKey manifest_store.ManifestKey
	pieceID     string
	body        []byte
	objectKey   string
}

func (v *Verifier) seedSteady(ctx context.Context) ([]seededObject, error) {
	return v.seedRange(ctx, 0, v.SteadyObjects, "steady")
}

func (v *Verifier) seedInFlight(ctx context.Context) ([]seededObject, error) {
	return v.seedRange(ctx, v.SteadyObjects, v.SteadyObjects+v.InFlightObjects, "in-flight")
}

// seedRange PUTs (end-start) objects into the source cell with
// async replication policy. The object key encodes the index so
// the destination-side GET can dedup objects across the two
// phases. The body is deterministic per index for byte-equality
// checks downstream.
func (v *Verifier) seedRange(ctx context.Context, start, end int, tag string) ([]seededObject, error) {
	out := make([]seededObject, 0, end-start)
	for i := start; i < end; i++ {
		objKey := fmt.Sprintf("%s/obj-%06d", tag, i)
		body := deterministicBody(objKey, v.ObjectSize)
		pieceID := fmt.Sprintf("piece-%s-%06d", tag, i)
		if _, err := v.Source.Provider.PutPiece(ctx, pieceID, bytes.NewReader(body), providers.PutOptions{
			ContentLength: int64(len(body)),
		}); err != nil {
			return nil, fmt.Errorf("source PutPiece(%s): %w", objKey, err)
		}
		hash := sha256Hex(objKey)
		m := &metadata.ObjectManifest{
			TenantID:      v.TenantID,
			Bucket:        v.Bucket,
			ObjectKey:     objKey,
			ObjectKeyHash: hash,
			VersionID:     "v1",
			ObjectSize:    int64(len(body)),
			ChunkSize:     int64(len(body)),
			Pieces: []metadata.Piece{{
				PieceID:   pieceID,
				Backend:   v.Source.ID,
				SizeBytes: int64(len(body)),
			}},
			PlacementPolicy: metadata.PlacementPolicy{
				AllowedBackends: []string{v.Source.ID, v.Dest.ID},
				ReplicationPolicy: &metadata.ReplicationPolicy{
					SourceCell: v.Source.ID,
					DestCell:   v.Dest.ID,
					Mode:       "async",
				},
			},
		}
		mkey := manifest_store.ManifestKey{
			TenantID:      m.TenantID,
			Bucket:        m.Bucket,
			ObjectKeyHash: m.ObjectKeyHash,
			VersionID:     m.VersionID,
		}
		if err := v.Source.Manifests.Put(ctx, mkey, m); err != nil {
			return nil, fmt.Errorf("source Manifests.Put(%s): %w", objKey, err)
		}
		out = append(out, seededObject{
			manifestKey: mkey,
			pieceID:     pieceID,
			body:        body,
			objectKey:   objKey,
		})
	}
	return out, nil
}

// waitForDrain polls the destination cell until every manifest
// in expected has landed (or LagSettleTimeout elapses). The
// drain signal is per-key Get rather than a List+count for two
// reasons:
//
//  1. countDestManifests counts ALL manifests in the (tenant,
//     bucket) scope, including any manifest pre-existing in
//     dst before Run() started. A test that pre-stages a
//     leaked-in-flight manifest in dst (e.g.
//     TestVerifier_LeakedInFlightFailsRun) would race against
//     the replicator: count >= expected can become true after
//     only (expected - prestaged) steady objects have actually
//     been copied. Phase 2 then cancels the replicator and
//     verifyRecovery fails with "steady-object recovery short"
//     instead of reaching the Phase-4 leak probe. Per-key Get
//     is immune to pre-existing dst manifests at unrelated
//     keys.
//
//  2. The timeout error becomes much more diagnostic — we can
//     report exactly which steady keys are still missing rather
//     than a single "count short by N" number.
//
// LagNanos cannot be used either because it resets to a
// non-zero value every tick.
func (v *Verifier) waitForDrain(ctx context.Context, repl *cross_cell.Replicator, expected []seededObject) error {
	// If Source and Dest share a manifest store the replicator
	// intentionally skips the Put (see replicator.go:200-202)
	// and the keys appear in dst the moment seedRange writes
	// them to src. Per-key Get is useless in that case, so fall
	// back to CopiedPieces — which starts at 0 on a fresh
	// Replicator and counts only pieces this run drained.
	//
	// The shared-store comparison "CopiedPieces >= len(expected)"
	// depends on the invariant that seedRange writes exactly one
	// piece per manifest (see seedRange's PutPiece+Manifests.Put
	// pair above). If a future change introduces multi-piece
	// manifests (EC, multipart), this branch must switch to
	// counting drained manifests rather than drained pieces, or
	// the drain check could pass before all manifests are
	// actually present in dst. The non-shared-store branch below
	// is immune because it queries per-manifest.
	sharedStore := v.Source.Manifests == v.Dest.Manifests

	deadline := time.NewTimer(v.LagSettleTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			missing := v.missingKeys(ctx, expected)
			return fmt.Errorf(
				"steady drain timed out after %s: missing=%d/%d, copied=%d, shared_store=%v, first_missing=%s",
				v.LagSettleTimeout, len(missing), len(expected), repl.CopiedPieces(), sharedStore,
				firstMissingObjectKey(missing),
			)
		case <-tick.C:
			if sharedStore {
				if repl.CopiedPieces() >= int64(len(expected)) {
					return nil
				}
				continue
			}
			done, err := v.allDestManifestsPresent(ctx, expected)
			if err != nil {
				return fmt.Errorf("check dest manifests: %w", err)
			}
			if done {
				return nil
			}
		}
	}
}

// allDestManifestsPresent returns true once every expected
// manifest key resolves in the destination cell. A NOT-FOUND on
// any key fails the check by returning false with a nil error
// (the caller's polling loop will retry on the next tick). Any
// other error from the store surfaces immediately so a flaky
// dest store cannot be mistaken for a slow replicator.
func (v *Verifier) allDestManifestsPresent(ctx context.Context, expected []seededObject) (bool, error) {
	for _, obj := range expected {
		_, err := v.Dest.Manifests.Get(ctx, obj.manifestKey)
		if err == nil {
			continue
		}
		if errors.Is(err, manifest_store.ErrNotFound) {
			return false, nil
		}
		return false, fmt.Errorf("dest Manifests.Get(%s): %w", obj.objectKey, err)
	}
	return true, nil
}

// missingKeys returns the subset of expected keys that did NOT
// resolve in the dest cell. Used to build a diagnostic timeout
// message — store errors are swallowed because the timeout path
// is purely cosmetic at that point.
func (v *Verifier) missingKeys(ctx context.Context, expected []seededObject) []seededObject {
	out := make([]seededObject, 0, len(expected))
	for _, obj := range expected {
		if _, err := v.Dest.Manifests.Get(ctx, obj.manifestKey); err != nil {
			out = append(out, obj)
		}
	}
	return out
}

func firstMissingObjectKey(missing []seededObject) string {
	if len(missing) == 0 {
		return ""
	}
	return missing[0].objectKey
}

// verifyRecovery drives the recovery phase: it GETs every
// steady-object body from the destination cell, asserts byte
// equality, captures the first-successful-GET timestamp for the
// RTO measurement, and records counts on the report.
//
// A backend regression — a piece in the destination manifest
// still pointing at the source cell — counts as a per-object
// failure, not a soft warning. The replicator's contract is to
// rewrite every piece's Backend on copy; a violation means the
// production GET path would route to the wrong backend and serve
// stale data, so the verifier must treat the object as
// unrecovered. This is the architecturally correct behaviour
// rather than appending to a mismatches log and quietly counting
// the object as recovered when the body happens to match.
//
// The function also defers populating RecoveryReadyAt / MeasuredRTO
// until it has confirmed every steady object was recovered. A
// JSON consumer reading the artifact for a failed run gets a zero
// RTO (the natural sentinel for "no recovery happened") rather
// than the timestamp of whichever GET happened to succeed before
// the run gave up.
func (v *Verifier) verifyRecovery(
	ctx context.Context,
	rpt *Report,
	steady []seededObject,
	now func() time.Time,
) error {
	var firstGetAt time.Time
	var mismatches []string

	for _, obj := range steady {
		// Look up the manifest by (tenant, bucket, object_key_hash, version_id).
		m, err := v.Dest.Manifests.Get(ctx, obj.manifestKey)
		if err != nil {
			mismatches = append(mismatches,
				fmt.Sprintf("%s: dest manifest missing: %v", obj.objectKey, err))
			continue
		}
		if len(m.Pieces) == 0 {
			mismatches = append(mismatches,
				fmt.Sprintf("%s: dest manifest has zero pieces", obj.objectKey))
			continue
		}
		// The replicator rewrites every piece's Backend to the
		// destination cell ID. A piece still pointing at the
		// source cell is a replicator regression — the production
		// GET path would route to the wrong backend. Treat the
		// object as unrecovered so RecoveredObjects is short of
		// SteadyObjects and the run fails loudly.
		backendOK := true
		for _, p := range m.Pieces {
			if p.Backend != v.Dest.ID {
				mismatches = append(mismatches, fmt.Sprintf(
					"%s: dest piece %s backend=%q want %q",
					obj.objectKey, p.PieceID, p.Backend, v.Dest.ID,
				))
				backendOK = false
			}
		}
		if !backendOK {
			continue
		}

		rc, err := v.Dest.Provider.GetPiece(ctx, m.Pieces[0].PieceID, nil)
		if err != nil {
			mismatches = append(mismatches,
				fmt.Sprintf("%s: dest GetPiece: %v", obj.objectKey, err))
			continue
		}
		if firstGetAt.IsZero() {
			// RTO is measured to the first successful piece OPEN,
			// not to the first byte-validated body. Rationale: the
			// production-meaningful event is "the dest cell served
			// a recovery read"; ReadAll + bytes.Equal happen below
			// and are guarded separately. The downstream gates at
			// the bottom of this function (firstGetAt.IsZero() and
			// RecoveredObjects < SteadyObjects) suppress
			// MeasuredRTO whenever the run did not fully recover,
			// so a JSON consumer can rely on MeasuredRTO > 0
			// meaning "every steady object was recovered".
			firstGetAt = now()
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			mismatches = append(mismatches,
				fmt.Sprintf("%s: read recovered body: %v", obj.objectKey, err))
			continue
		}
		if !bytes.Equal(body, obj.body) {
			mismatches = append(mismatches, fmt.Sprintf(
				"%s: body mismatch (want %d bytes, got %d bytes; hash want=%s got=%s)",
				obj.objectKey, len(obj.body), len(body),
				sha256Hex(string(obj.body))[:12], sha256Hex(string(body))[:12],
			))
			continue
		}
		rpt.RecoveredObjects++
	}

	if firstGetAt.IsZero() {
		// No successful GET happened. Leave RecoveryReadyAt and
		// MeasuredRTO at zero — a consumer reading the JSON report
		// can rely on a non-zero MeasuredRTO meaning "recovery
		// succeeded".
		return fmt.Errorf(
			"recovery failed: 0/%d steady objects recoverable from dest cell (%d mismatches: %v)",
			len(steady), len(mismatches), mismatches,
		)
	}

	if rpt.RecoveredObjects < v.SteadyObjects {
		// Partial recovery is still a failed run from a DR
		// standpoint; suppress the RTO measurement so the JSON
		// artifact does not advertise a recovery time for a run
		// that left objects unreachable.
		return fmt.Errorf(
			"steady-object recovery short: recovered %d/%d (mismatches: %v)",
			rpt.RecoveredObjects, v.SteadyObjects, mismatches,
		)
	}

	// Sanity-check: an empty mismatches slice is invariant when
	// RecoveredObjects == SteadyObjects, but if a future refactor
	// adds a path that appends to mismatches without decrementing
	// RecoveredObjects we still want the run to fail loudly
	// rather than silently masking the problem.
	if len(mismatches) > 0 {
		return fmt.Errorf(
			"recovery accounting drift: RecoveredObjects=%d == SteadyObjects=%d but %d mismatches were recorded: %v",
			rpt.RecoveredObjects, v.SteadyObjects, len(mismatches), mismatches,
		)
	}

	rpt.RecoveryReadyAt = firstGetAt
	rpt.MeasuredRTO = firstGetAt.Sub(rpt.FailureDetectedAt)
	return nil
}

// measureRPO probes the destination cell for each in-flight
// manifest and returns (lost, leaked, error). "lost" is the
// number of in-flight manifests NOT present in the destination
// (the value reported as MeasuredRPO). "leaked" is the number
// present in the destination despite the replicator being
// cancelled before the in-flight seed — a non-zero value here
// means the Phase-2 ordering invariant was broken and the caller
// should surface it as an error.
//
// The verifier asserts "the replicator did not drain any in-flight
// pieces" by measurement, not by construction, so a future
// refactor that breaks the cancel-before-seed ordering surfaces
// here rather than silently inflating MeasuredRPO to the
// configured InFlightObjects value.
func (v *Verifier) measureRPO(ctx context.Context, inFlight []seededObject) (lost, leaked int, err error) {
	for _, obj := range inFlight {
		if _, getErr := v.Dest.Manifests.Get(ctx, obj.manifestKey); getErr != nil {
			if errors.Is(getErr, manifest_store.ErrNotFound) {
				lost++
				continue
			}
			return 0, 0, fmt.Errorf("dest Manifests.Get(%s): %w", obj.objectKey, getErr)
		}
		leaked++
	}
	return lost, leaked, nil
}

func deterministicBody(objKey string, size int) []byte {
	// Deterministic per-key body: derived from SHA-256 of the
	// key, tiled up to `size`. Keys differ so bodies differ —
	// a content-based dedup engine would still treat them as
	// distinct.
	seed := sha256.Sum256([]byte(objKey))
	out := make([]byte, size)
	for i := 0; i < size; i += len(seed) {
		copy(out[i:], seed[:])
	}
	return out
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

