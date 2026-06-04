package chaos

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/repair"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration/lazy_read_repair"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// scenarioFixture is the shared setup for every chaos scenario that
// drives the repair queue. It builds a real local_fs_dev provider,
// EC-encodes a small plaintext, persists every shard, deletes one
// shard to make it "degraded", and returns the manifest plus a
// FaultProvider wrapping the backing provider so the test can inject
// faults on top of the same real data plane the queue would touch in
// production.
type scenarioFixture struct {
	t            *testing.T
	backing      providers.StorageProvider
	fault        *FaultProvider
	encoder      *erasure_coding.Encoder
	manifest     *metadata.ObjectManifest
	plaintext    []byte
	shards       []erasure_coding.Shard
	degradedID   string
	profile      string
	store        manifest_store.ManifestStore
	registry     *erasure_coding.Registry
	providerName string
}

func newScenarioFixture(t *testing.T) *scenarioFixture {
	t.Helper()
	tmp := t.TempDir()
	backing, err := local_fs_dev.New(filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	reg := erasure_coding.DefaultRegistry()
	profile := reg.Names()[0]
	enc, err := reg.Lookup(profile)
	if err != nil {
		t.Fatalf("registry.Lookup(%q): %v", profile, err)
	}
	plaintext := bytes.Repeat([]byte("zk-object-fabric:chaos-scenario\n"), 96)
	shards, err := enc.Encode(plaintext)
	if err != nil {
		t.Fatalf("encoder.Encode: %v", err)
	}
	ctx := context.Background()
	pieces := make([]metadata.Piece, 0, len(shards))
	for i, sh := range shards {
		pid := fmt.Sprintf("chaos-shard-%02d", i)
		pieces = append(pieces, metadata.Piece{
			PieceID:     pid,
			Backend:     "chaos-backend",
			StripeIndex: sh.StripeIndex,
			ShardIndex:  sh.ShardIndex,
			ShardKind:   sh.Kind.String(),
			SizeBytes:   int64(len(sh.Bytes)),
		})
		if _, err := backing.PutPiece(ctx, pid, bytes.NewReader(sh.Bytes), providers.PutOptions{ContentLength: int64(len(sh.Bytes))}); err != nil {
			t.Fatalf("seed put: %v", err)
		}
	}
	degraded := pieces[0].PieceID
	if err := backing.DeletePiece(ctx, degraded); err != nil {
		t.Fatalf("seed delete: %v", err)
	}
	m := &metadata.ObjectManifest{
		TenantID:      "T",
		Bucket:        "b",
		ObjectKey:     "chaos-object",
		ObjectKeyHash: "chaos-object",
		ObjectSize:    int64(len(plaintext)),
		Pieces:        pieces,
		PlacementPolicy: metadata.PlacementPolicy{
			ErasureProfile: profile,
		},
	}
	store := memory.New()
	if err := store.Put(ctx, manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "chaos-object"}, m); err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	fp := NewFaultProvider(backing)
	return &scenarioFixture{
		t:            t,
		backing:      backing,
		fault:        fp,
		encoder:      enc,
		manifest:     m,
		plaintext:    plaintext,
		shards:       shards,
		degradedID:   degraded,
		profile:      profile,
		store:        store,
		registry:     reg,
		providerName: "chaos-backend",
	}
}

// queueWaitMode controls runQueueUntil's exit condition.
type queueWaitMode int

const (
	// waitForRepaired returns as soon as q.RepairedCount() >= want,
	// or when ctx fires. Use this for scenarios where the chaos is
	// supposed to subside and the queue is expected to recover.
	waitForRepaired queueWaitMode = iota
	// waitForAttempts returns as soon as
	// repaired+failed >= want. Use this for scenarios where the
	// chaos is permanent and we just want to confirm the queue
	// observed the failure.
	waitForAttempts
)

// runQueueUntil drives the repair queue with a short poll interval
// until the specified mode's exit condition is satisfied or ctx
// fires. Returns the final (repaired, failed) counts.
func runQueueUntil(t *testing.T, q *repair.RepairQueue, want int64, mode queueWaitMode, ctx context.Context) (repaired, failed int64) {
	t.Helper()
	q.PollInterval = 5 * time.Millisecond
	done := make(chan struct{})
	go func() {
		_ = q.Run(ctx)
		close(done)
	}()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		r, f := q.RepairedCount(), q.FailedCount()
		switch mode {
		case waitForRepaired:
			if r >= want {
				return r, f
			}
		case waitForAttempts:
			if r+f >= want {
				return r, f
			}
		}
		select {
		case <-done:
			return q.RepairedCount(), q.FailedCount()
		case <-deadline.C:
			return q.RepairedCount(), q.FailedCount()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestChaos_RepairRecoversFromProviderFailFirstNStorm models a
// transient Wasabi 503 storm: the first N writes back fail, then the
// origin recovers. The repair queue must persist across polls and
// eventually re-encode the degraded shard.
//
// This is a real chaos scenario, not a unit-test mock: the repair
// queue runs against the actual erasure_coding.Encoder, the actual
// memory.ManifestStore, and a real local_fs_dev provider, with the
// FaultProvider injecting failures on the PUT path the queue uses
// to write back the re-encoded shard.
func TestChaos_RepairRecoversFromProviderFailFirstNStorm(t *testing.T) {
	fx := newScenarioFixture(t)
	wantInjErr := errors.New("chaos: wasabi 503 storm")
	fx.fault.PutFault = FaultConfig{
		Mode:   ModeFailFirstN,
		Err:    wantInjErr,
		FirstN: 2, // first 2 put-backs fail, then succeed
	}

	q := repair.NewRepairQueue(
		&fakeHealthSource{signal: repair.HealthSignal{Healthy: false, AffectedPieceIDs: []string{fx.degradedID}}},
		&fakeManifestScanner{manifests: []*metadata.ObjectManifest{fx.manifest}},
		fx.store,
		map[string]providers.StorageProvider{fx.providerName: fx.fault},
		fx.registry,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	repaired, failed := runQueueUntil(t, q, 1, waitForRepaired, ctx)

	if repaired < 1 {
		t.Fatalf("repair never recovered: repaired=%d failed=%d storm_attempts=%d",
			repaired, failed, fx.fault.Failures.Load())
	}
	if fx.fault.Failures.Load() < int64(fx.fault.PutFault.FirstN) {
		t.Errorf("fault injector triggered %d times, want at least %d "+
			"(the storm should have consumed FirstN put attempts before recovery)",
			fx.fault.Failures.Load(), fx.fault.PutFault.FirstN)
	}

	// And the re-encoded shard must be byte-identical to the
	// original — repair must not silently rewrite different data.
	rc, err := fx.backing.GetPiece(context.Background(), fx.degradedID, nil)
	if err != nil {
		t.Fatalf("repaired piece missing: %v", err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if !bytes.Equal(body, fx.shards[0].Bytes) {
		t.Errorf("repaired body length=%d differs from original shard length=%d",
			len(body), len(fx.shards[0].Bytes))
	}
}

// TestChaos_RepairSurvivesTruncatedSurvivorRead models a partition
// mid-GET: one of the survivor shards is delivered truncated, which
// looks like a partial read followed by an error to the queue. The
// queue must still repair using the remaining survivors (EC profile
// has enough redundancy).
//
// Crucial property: the queue must NOT write a phantom piece back to
// the provider on the basis of the truncated survivor. We verify
// that the repaired piece (the originally-deleted shard) is restored
// to its true byte sequence, not a corrupted derivation.
func TestChaos_RepairSurvivesTruncatedSurvivorRead(t *testing.T) {
	fx := newScenarioFixture(t)
	// Pick a survivor (not the already-deleted shard) and serve a
	// truncated body on its first read. The repair queue calls
	// io.ReadAll on the survivor; ReadAll will return partial
	// bytes + our injected error. The fetchPiece path returns the
	// error, the queue logs and treats the survivor as missing,
	// and the encoder reconstructs from the remaining shards.
	fx.fault.GetFault = FaultConfig{
		Mode:               ModeTruncatedRead,
		Err:                errors.New("chaos: tcp reset mid-read"),
		TruncateAfterBytes: 4, // 4 bytes is far below any real shard size
	}

	q := repair.NewRepairQueue(
		&fakeHealthSource{signal: repair.HealthSignal{Healthy: false, AffectedPieceIDs: []string{fx.degradedID}}},
		&fakeManifestScanner{manifests: []*metadata.ObjectManifest{fx.manifest}},
		fx.store,
		map[string]providers.StorageProvider{fx.providerName: fx.fault},
		fx.registry,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	repaired, failed := runQueueUntil(t, q, 1, waitForAttempts, ctx)

	// The EC profile (default) has parity > 0, so a single
	// truncated survivor still leaves enough shards to decode.
	// We accept either repaired>=1 (decoded around the truncation)
	// or failed>=1 (queue tried, decoder didn't have enough). The
	// hard requirement is: no panic, no orphaned write, no
	// silent corruption.
	if repaired+failed < 1 {
		t.Fatalf("queue never observed the manifest: repaired=%d failed=%d", repaired, failed)
	}
	if repaired >= 1 {
		rc, err := fx.backing.GetPiece(context.Background(), fx.degradedID, nil)
		if err != nil {
			t.Fatalf("repaired piece missing despite repaired=%d: %v", repaired, err)
		}
		defer rc.Close()
		body, _ := io.ReadAll(rc)
		if !bytes.Equal(body, fx.shards[0].Bytes) {
			t.Errorf("repaired body differs from original shard "+
				"(len got=%d want=%d) — truncated survivor read "+
				"may have led to silent corruption",
				len(body), len(fx.shards[0].Bytes))
		}
	}
}

// TestChaos_RepairGracefullyFailsWhenProviderAlwaysDown models a
// hard outage: GetPiece always errors. The repair queue must mark
// the attempt as failed (FailedCount > 0), must NOT panic, and must
// NOT mutate the underlying provider on the way out.
func TestChaos_RepairGracefullyFailsWhenProviderAlwaysDown(t *testing.T) {
	fx := newScenarioFixture(t)
	fx.fault.GetFault = FaultConfig{
		Mode: ModeAlwaysFail,
		Err:  errors.New("chaos: backend totally unreachable"),
	}
	// Capture the pre-chaos state of the provider so we can
	// assert nothing was mutated by the failed repair attempt.
	preList, err := fx.backing.ListPieces(context.Background(), "", "")
	if err != nil {
		t.Fatalf("pre-chaos list: %v", err)
	}

	q := repair.NewRepairQueue(
		&fakeHealthSource{signal: repair.HealthSignal{Healthy: false, AffectedPieceIDs: []string{fx.degradedID}}},
		&fakeManifestScanner{manifests: []*metadata.ObjectManifest{fx.manifest}},
		fx.store,
		map[string]providers.StorageProvider{fx.providerName: fx.fault},
		fx.registry,
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	repaired, failed := runQueueUntil(t, q, 1, waitForAttempts, ctx)
	if failed < 1 {
		t.Fatalf("queue should record at least 1 failed attempt; got "+
			"repaired=%d failed=%d. A hard backend outage MUST surface "+
			"as a recorded failure, not a silent no-op.", repaired, failed)
	}
	if repaired != 0 {
		t.Errorf("queue claimed repaired=%d under a completely dead "+
			"backend; impossible without writing real bytes", repaired)
	}
	// Provider state must be unchanged. The degraded piece is
	// still absent; the survivors are still present and unmodified.
	postList, err := fx.backing.ListPieces(context.Background(), "", "")
	if err != nil {
		t.Fatalf("post-chaos list: %v", err)
	}
	if len(postList.Pieces) != len(preList.Pieces) {
		t.Errorf("provider piece count changed under failed repair: "+
			"pre=%d post=%d (failed repair must be atomic — no orphan "+
			"writes, no phantom shards)", len(preList.Pieces), len(postList.Pieces))
	}
}

// TestChaos_RepairRespectsContextCancellationUnderSlowProvider
// models a degraded backend that does not error but is slow enough
// to blow through the per-repair deadline. The queue must observe
// the cancellation between operations and exit cleanly — not deadlock
// waiting for a hung provider, not keep accumulating goroutines.
func TestChaos_RepairRespectsContextCancellationUnderSlowProvider(t *testing.T) {
	fx := newScenarioFixture(t)
	fx.fault.GetFault = FaultConfig{
		Mode:    ModeSlowResponse,
		Latency: 200 * time.Millisecond,
	}
	fx.fault.PutFault = FaultConfig{
		Mode:    ModeSlowResponse,
		Latency: 200 * time.Millisecond,
	}

	q := repair.NewRepairQueue(
		&fakeHealthSource{signal: repair.HealthSignal{Healthy: false, AffectedPieceIDs: []string{fx.degradedID}}},
		&fakeManifestScanner{manifests: []*metadata.ObjectManifest{fx.manifest}},
		fx.store,
		map[string]providers.StorageProvider{fx.providerName: fx.fault},
		fx.registry,
	)
	q.PollInterval = 5 * time.Millisecond

	// Give the queue strictly less time than a single repair would
	// need to complete (1 GET per surviving shard + 1 PUT per
	// degraded shard, each 200ms). Run().poll() at least starts;
	// cancellation must be observed before it finishes the slow
	// reads.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := q.Run(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run returned %v; want context.DeadlineExceeded", err)
	}
	// A naive implementation that did not check ctx between
	// operations would block for at least one GET + one PUT
	// (>= 400ms). The queue must exit much faster.
	if elapsed > 1*time.Second {
		t.Errorf("Run took %v under slow provider; expected to exit "+
			"shortly after ctx deadline (150ms). Suggests the queue "+
			"is not cancelling its in-flight backend calls or is "+
			"hanging on a goroutine.", elapsed)
	}
}

// TestChaos_ManifestStorePutFailsLoudlyToCaller models a Postgres
// primary outage during a write. The store must surface a concrete
// error to the caller — never silently succeed, never duplicate a
// stale read, never partial-write. This is the property the gateway
// relies on to refuse a PUT that did not durably commit a manifest.
func TestChaos_ManifestStorePutFailsLoudlyToCaller(t *testing.T) {
	inner := memory.New()
	want := errors.New("chaos: postgres primary down")
	fms := NewFaultManifestStore(inner)
	fms.PutFault = FaultConfig{Mode: ModeAlwaysFail, Err: want}

	key := manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "k"}
	m := &metadata.ObjectManifest{TenantID: "T", Bucket: "b", ObjectKey: "k", ObjectKeyHash: "k"}

	// Every Put across 16 concurrent callers must error.
	const n = 16
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- fms.Put(context.Background(), key, m)
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		if !errors.Is(e, want) {
			t.Errorf("Put returned %v; want errors.Is(%v) true", e, want)
		}
	}
	// And the underlying store must have observed ZERO writes.
	// A silent commit would be worse than the loud failure: the
	// gateway would have already told the client "200 OK" while
	// the data was lost.
	got, err := inner.Get(context.Background(), key)
	if err == nil {
		t.Errorf("inner.Get succeeded under failing wrapper: got=%v "+
			"— the wrapper must not allow any Put through when "+
			"PutFault.Mode=ModeAlwaysFail", got)
	}
}

// TestChaos_ConcurrentProviderAndManifestStoreFailureDegrades models
// a worst-case partition: both the data backend AND the manifest
// store are degraded at the same time. A caller-side write loop
// must observe loud errors on the manifest path, the data backend
// must not be partial-mutated by the failed flow, and the overall
// system must remain coherent (no half-written shard / dangling
// manifest reference).
func TestChaos_ConcurrentProviderAndManifestStoreFailureDegrades(t *testing.T) {
	// Real provider + manifest store wrapped in fault injectors.
	tmp := t.TempDir()
	backing, err := local_fs_dev.New(filepath.Join(tmp, "data"))
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	fp := NewFaultProvider(backing)
	fp.PutFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 2, Err: errors.New("chaos: backend half-down")}

	inner := memory.New()
	fms := NewFaultManifestStore(inner)
	fms.PutFault = FaultConfig{Mode: ModeFailEveryNth, EveryNth: 3, Err: errors.New("chaos: pg primary unreachable")}

	// Driver loop: simulate the gateway's "write a piece, then
	// commit the manifest" 2-step PUT. The chaos is that EITHER
	// step can fail independently. The invariant we are checking:
	// for every observed successful write, BOTH steps must have
	// succeeded; for every failure, the system must not have
	// half-committed (no orphan piece + no orphan manifest).
	const total = 30
	var committed int
	var piecesOrphaned int
	for i := 0; i < total; i++ {
		pid := fmt.Sprintf("multi-fail-piece-%02d", i)
		body := []byte(fmt.Sprintf("payload-%02d", i))
		_, err := fp.PutPiece(context.Background(), pid, bytes.NewReader(body), providers.PutOptions{ContentLength: int64(len(body))})
		if err != nil {
			// Piece write failed; manifest must not commit.
			// Verify the provider did not partial-store the
			// piece either (HEAD must say not found).
			if _, headErr := backing.HeadPiece(context.Background(), pid); headErr == nil {
				t.Errorf("piece %s reported a put error but HEAD found "+
					"it; FaultProvider must not partial-mutate the "+
					"backing store on a faulted PUT", pid)
			}
			continue
		}
		mfErr := fms.Put(context.Background(),
			manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: pid},
			&metadata.ObjectManifest{
				TenantID: "T", Bucket: "b", ObjectKey: pid, ObjectKeyHash: pid,
				Pieces: []metadata.Piece{{PieceID: pid, Backend: "x", SizeBytes: int64(len(body))}},
			})
		if mfErr != nil {
			// Manifest write failed AFTER piece committed — this
			// is the orphaned-piece case. A production gateway
			// would either roll back the piece or enqueue it for
			// GC. For the chaos suite we just account for it so
			// the test surfaces the count.
			piecesOrphaned++
			continue
		}
		committed++
	}

	if committed == 0 {
		t.Fatalf("no PUT survived both fault layers; the EveryNth "+
			"cadences (2 and 3) should yield ~ (1/2)*(2/3)=33%% successful "+
			"writes — got 0. fp.Failures=%d fms.Failures=%d",
			fp.Failures.Load(), fms.Failures.Load())
	}
	if committed == total {
		t.Errorf("all %d PUTs succeeded under EveryNth(2)+EveryNth(3) "+
			"fault wrappers; that is statistically impossible and "+
			"means the wrappers were not actually engaged", total)
	}
	// piecesOrphaned > 0 is acceptable here — the test is making
	// the explicit point that without an upstream rollback, the
	// 2-step write IS partial under concurrent fault. The cell
	// operator runbook (Workstream 1.6) must address this with a
	// reconciler. Logging the count lets a future change surface
	// any regression of the rate.
	t.Logf("multi-fault summary: committed=%d failed=%d orphaned=%d "+
		"fp_failures=%d fms_failures=%d",
		committed, total-committed-piecesOrphaned, piecesOrphaned,
		fp.Failures.Load(), fms.Failures.Load())
}

// TestChaos_FaultManifestStoreEventualHealReleasesBackpressure
// models the manifest tier coming back online after a timed outage.
// While the tier is down, every Put errors loudly; after the
// deadline, Puts pass through and the backlog clears.
//
// Uses fms.Now to drive a synthetic clock so the test is
// deterministic under heavy CI load (no wall-clock sleeps). The
// real ModeFailUntilTime code path consults fms.now() exactly the
// same way it would under time.Now, so this fully exercises the
// production code.
func TestChaos_FaultManifestStoreEventualHealReleasesBackpressure(t *testing.T) {
	inner := memory.New()
	fms := NewFaultManifestStore(inner)

	// Synthetic clock starts at t=0; heal at t=50ms.
	clockBase := time.Unix(1700000000, 0)
	var clockOffset atomic.Int64 // nanoseconds since clockBase
	fms.Now = func() time.Time {
		return clockBase.Add(time.Duration(clockOffset.Load()))
	}
	healAt := clockBase.Add(50 * time.Millisecond)
	fms.PutFault = FaultConfig{
		Mode:      ModeFailUntilTime,
		Err:       errors.New("chaos: pg failover in progress"),
		FailUntil: healAt,
	}

	key := manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "k"}
	m := &metadata.ObjectManifest{TenantID: "T", Bucket: "b", ObjectKey: "k", ObjectKeyHash: "k"}

	// During the outage window every Put must fail. Advance the
	// synthetic clock between attempts to mimic the original
	// wall-clock cadence (2ms per probe) without any real sleeps.
	for i := 0; i < 5; i++ {
		if err := fms.Put(context.Background(), key, m); err == nil {
			t.Fatalf("Put attempt %d succeeded during outage window; "+
				"FailUntilTime must keep failing every call before "+
				"the deadline (synthetic clock at +%dns)",
				i, clockOffset.Load())
		}
		clockOffset.Add(int64(2 * time.Millisecond))
	}

	// Jump past the heal point — no time.Sleep needed.
	clockOffset.Store(int64(60 * time.Millisecond))
	if err := fms.Put(context.Background(), key, m); err != nil {
		t.Fatalf("Put after heal returned %v; want nil "+
			"(FailUntilTime should release at the deadline; "+
			"synthetic clock at +%dns >= FailUntil)",
			err, clockOffset.Load())
	}
	// And the stored manifest must be retrievable.
	if got, err := inner.Get(context.Background(), key); err != nil || got == nil {
		t.Errorf("Get after heal: err=%v got=%v; want a stored manifest "+
			"— the post-heal Put must commit through the wrapper", err, got)
	}
}

// TestChaos_MetadataDBFailover injects ManifestStore failures
// mid-PUT and mid-GET via FaultManifestStore and asserts the gateway
// fails closed with a 5xx (never a 2xx, never corrupted/partial data)
// and recovers cleanly once the store is restored.
//
// Production-readiness note: the ideal mapping for a transient
// metadata-store outage is a retryable 503 (S3 ServiceUnavailable) so
// clients back off and retry. The gateway today maps a manifest-store
// read error to 500 and a write error to 500 (see resolve() →
// ManifestGetFailed and Put() → ManifestPutFailed in
// api/s3compat/handler.go). This test asserts the invariants that
// actually hold on current main — fail-closed 5xx + clean recovery +
// no orphaned object from a failed PUT — and records the 500→503 gap
// rather than pinning a status code the gateway does not yet return.
func TestChaos_MetadataDBFailover(t *testing.T) {
	inner := memory.New()
	fms := NewFaultManifestStore(inner)
	backing := newChaosBacking(t)
	h := newChaosGateway(t, s3compat.Config{
		Manifests: fms,
		Providers: map[string]providers.StorageProvider{"local": backing},
		Placement: chaosPlacement{backend: "local"},
	})

	const bucket, key = "b", "k1"
	body := []byte("metadata-failover-payload")

	// Phase 1: healthy store. PUT then GET must round-trip.
	if rec := gwPut(t, h, bucket, key, body); rec.Code != http.StatusOK {
		t.Fatalf("healthy PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if rec := gwGet(t, h, bucket, key); rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("healthy GET status=%d body=%q, want 200 %q", rec.Code, rec.Body.String(), body)
	}

	// Phase 2: the metadata store's read path fails (primary down).
	// The GET must fail closed with a server error and must NOT leak
	// the object bytes.
	fms.GetFault = FaultConfig{Mode: ModeAlwaysFail, Err: errors.New("chaos: postgres primary unreachable")}
	rec := gwGet(t, h, bucket, key)
	if rec.Code/100 != 5 {
		t.Fatalf("GET under metadata-read failure status = %d, want a 5xx server error", rec.Code)
	}
	if strings.Contains(rec.Body.String(), string(body)) {
		t.Errorf("GET under metadata failure leaked object bytes in the error body: %q", rec.Body.String())
	}
	t.Logf("metadata-read failure → status %d (production-readiness target: 503 ServiceUnavailable, retryable)", rec.Code)

	// Phase 3: the metadata store's write path fails (primary
	// read-only). The PUT must fail closed AND must not leave an
	// orphaned object behind: the gateway rolls the piece back, so a
	// later GET of the never-committed key is a clean 404, not a 500
	// or a half-written object.
	fms.GetFault = FaultConfig{}
	fms.PutFault = FaultConfig{Mode: ModeAlwaysFail, Err: errors.New("chaos: postgres write path read-only")}
	const failedKey = "k2"
	if rec := gwPut(t, h, bucket, failedKey, []byte("never-commits")); rec.Code/100 != 5 {
		t.Fatalf("PUT under metadata-write failure status = %d, want a 5xx server error", rec.Code)
	}

	// Phase 4: the store heals. The original object is still
	// readable (no corruption from the outage) and the failed PUT
	// left nothing behind.
	fms.PutFault = FaultConfig{}
	if rec := gwGet(t, h, bucket, key); rec.Code != http.StatusOK || rec.Body.String() != string(body) {
		t.Fatalf("post-recovery GET of original status=%d body=%q, want 200 %q", rec.Code, rec.Body.String(), body)
	}
	if rec := gwGet(t, h, bucket, failedKey); rec.Code != http.StatusNotFound {
		t.Errorf("GET of the never-committed key status = %d, want 404 (the failed PUT must not orphan an object)", rec.Code)
	}
}

// TestChaos_CachePartition wedges the hot-object cache so every Get
// errors (L0+L1 both gone) via a nil-Inner FaultCache in
// ModeAlwaysFail, and asserts every request falls through to the
// origin provider and serves byte-correct data — the cache being
// useless must degrade latency, never correctness.
func TestChaos_CachePartition(t *testing.T) {
	store := memory.New()
	backing := newChaosBacking(t)
	cache := &FaultCache{
		GetFault: FaultConfig{Mode: ModeAlwaysFail, Err: errors.New("chaos: cache partition (L0+L1 down)")},
		PutFault: FaultConfig{Mode: ModeAlwaysFail, Err: errors.New("chaos: cache partition (L0+L1 down)")},
	}
	h := newChaosGateway(t, s3compat.Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"local": backing},
		Placement: chaosPlacement{backend: "local"},
		Cache:     cache,
	})

	const bucket, key = "b", "cached-obj"
	body := bytes.Repeat([]byte("partition-fallthrough-"), 64)
	if rec := gwPut(t, h, bucket, key, body); rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Every read must fall through to origin with correct bytes.
	const reads = 5
	for i := 0; i < reads; i++ {
		rec := gwGet(t, h, bucket, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %d under cache partition status = %d, want 200; body=%s", i, rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Fatalf("GET %d served wrong bytes under cache partition (len got=%d want=%d)", i, rec.Body.Len(), len(body))
		}
	}

	// The cache must actually have been consulted (proving the
	// fall-through path, not a disabled cache) and never served a
	// single hit.
	if cache.Calls.Load() == 0 {
		t.Error("cache was never consulted; the GET path did not exercise the cache fall-through")
	}
	if cache.Hits.Load() != 0 {
		t.Errorf("cache reported %d hits under a full partition, want 0", cache.Hits.Load())
	}
}

// TestChaos_WasabiTimeout injects a 30s delay on the provider GET/PUT
// path via FaultProvider (ModeSlowResponse) and asserts the gateway
// respects the caller's context deadline instead of hanging for the
// full latency, returning promptly with a 5xx.
//
// Production-readiness note: a backend that blows the request budget
// should surface as 504 GatewayTimeout. The gateway currently maps
// the resulting context error to 502 BackendGetFailed on the read
// path. This test asserts the property that matters operationally —
// the gateway does not pin a goroutine for 30s and fails promptly —
// and records the 502→504 gap.
func TestChaos_WasabiTimeout(t *testing.T) {
	store := memory.New()
	backing := newChaosBacking(t)
	fp := NewFaultProvider(backing)
	h := newChaosGateway(t, s3compat.Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"local": fp},
		Placement: chaosPlacement{backend: "local"},
	})

	const bucket, key = "b", "slow-obj"
	body := []byte("timeout-budget-payload")
	if rec := gwPut(t, h, bucket, key, body); rec.Code != http.StatusOK {
		t.Fatalf("seed PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// Inject a 30s delay on the GET path and give the request a
	// short deadline. The gateway must abort near the deadline, not
	// after 30s.
	fp.GetFault = FaultConfig{Mode: ModeSlowResponse, Latency: 30 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	start := time.Now()
	h.Get(rec, req)
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Fatalf("GET took %s under a 30s backend delay with a 200ms deadline; the gateway did not honour the caller timeout", elapsed)
	}
	if rec.Code/100 != 5 {
		t.Fatalf("GET under backend timeout status = %d, want a 5xx server error", rec.Code)
	}
	t.Logf("backend timeout → status %d after %s (production-readiness target: 504 GatewayTimeout)", rec.Code, elapsed)
}

// TestChaos_ConcurrentMigration runs a Wasabi→local_fs_dev migration
// (driven by the real lazy_read_repair.ReadRepair) while concurrent
// PUTs and GETs are in flight, and asserts zero data loss and no
// stale/corrupt reads: every migrated object ends up on the new
// primary with byte-identical content, every concurrent read returns
// correct bytes regardless of migration progress, and the
// concurrently-written objects are all durably stored.
func TestChaos_ConcurrentMigration(t *testing.T) {
	wasabi := newChaosBacking(t)
	local := newChaosBacking(t)
	store := memory.New()
	registry := map[string]providers.StorageProvider{"wasabi": wasabi, "local": local}
	rr := lazy_read_repair.New(registry, store)

	// Gateway whose objects live on "wasabi"; ReadRepair wired so a
	// read during migration can be served from either backend.
	hWasabi := newChaosGateway(t, s3compat.Config{
		Manifests:  store,
		Providers:  registry,
		Placement:  chaosPlacement{backend: "wasabi"},
		ReadRepair: rr,
	})
	// A second gateway that writes fresh objects to "local"
	// concurrently, modelling normal write traffic during migration.
	hLocal := newChaosGateway(t, s3compat.Config{
		Manifests: store,
		Providers: registry,
		Placement: chaosPlacement{backend: "local"},
	})

	const bucket = "b"
	const nObjects = 8

	// Seed migratable objects on wasabi, then mark each manifest as
	// mid-migration to "local" (Generation 2, PrimaryBackend local)
	// while the piece still physically lives on wasabi.
	type seeded struct {
		key  string
		body []byte
		mkey manifest_store.ManifestKey
	}
	objs := make([]seeded, nObjects)
	for i := range objs {
		key := fmt.Sprintf("mig-%d", i)
		body := []byte(fmt.Sprintf("migratable-object-%d-payload", i))
		if rec := gwPut(t, hWasabi, bucket, key, body); rec.Code != http.StatusOK {
			t.Fatalf("seed PUT %s status = %d, want 200; body=%s", key, rec.Code, rec.Body)
		}
		m := latestManifest(t, store, bucket, key)
		mk := manifest_store.ManifestKey{TenantID: m.TenantID, Bucket: m.Bucket, ObjectKeyHash: m.ObjectKeyHash, VersionID: m.VersionID}
		m.MigrationState = metadata.MigrationState{Generation: 2, PrimaryBackend: "local", CloudCopy: "wasabi"}
		if err := store.Put(context.Background(), mk, m); err != nil {
			t.Fatalf("mark migrating %s: %v", key, err)
		}
		objs[i] = seeded{key: key, body: body, mkey: mk}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	errCh := make(chan error, nObjects*8)

	// Migrators: copy each object's piece wasabi→local and flip the
	// manifest primary.
	for i := range objs {
		wg.Add(1)
		go func(o seeded) {
			defer wg.Done()
			// Look up the manifest inline (not via latestManifest,
			// which calls t.Fatalf): t.Fatal from a non-test
			// goroutine only kills the calling goroutine via
			// runtime.Goexit and silently swallows the failure, so
			// errors are funnelled through errCh like the reader and
			// writer goroutines below.
			m, err := store.Get(ctx, o.mkey)
			if err != nil {
				errCh <- fmt.Errorf("load manifest for repair %s: %w", o.key, err)
				return
			}
			if _, err := rr.Repair(ctx, o.mkey, m, 0); err != nil {
				errCh <- fmt.Errorf("repair %s: %w", o.key, err)
			}
		}(objs[i])
	}

	// Readers: hammer GETs of the migrating objects; every read must
	// return correct bytes whether it lands before or after the flip.
	for i := range objs {
		wg.Add(1)
		go func(o seeded) {
			defer wg.Done()
			for r := 0; r < 6; r++ {
				rec := gwGet(t, hWasabi, bucket, o.key)
				if rec.Code != http.StatusOK {
					errCh <- fmt.Errorf("concurrent GET %s status=%d body=%s", o.key, rec.Code, rec.Body)
					return
				}
				if !bytes.Equal(rec.Body.Bytes(), o.body) {
					errCh <- fmt.Errorf("concurrent GET %s served stale/corrupt bytes", o.key)
					return
				}
			}
		}(objs[i])
	}

	// Writers: create fresh objects on local during the migration.
	newBodies := make([][]byte, nObjects)
	for i := range newBodies {
		newBodies[i] = []byte(fmt.Sprintf("new-write-%d", i))
	}
	for i := range newBodies {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if rec := gwPut(t, hLocal, bucket, fmt.Sprintf("new-%d", idx), newBodies[idx]); rec.Code != http.StatusOK {
				errCh <- fmt.Errorf("concurrent PUT new-%d status=%d body=%s", idx, rec.Code, rec.Body)
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Post-conditions: every migrated object now lives on local with
	// byte-identical content, and reads still serve correct bytes.
	for _, o := range objs {
		m := latestManifest(t, store, bucket, o.key)
		if got := m.Pieces[0].Backend; got != "local" {
			t.Errorf("object %s piece backend = %q after migration, want local", o.key, got)
		}
		rc, err := local.GetPiece(context.Background(), m.Pieces[0].PieceID, nil)
		if err != nil {
			t.Errorf("migrated object %s missing on new primary: %v", o.key, err)
			continue
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		if !bytes.Equal(got, o.body) {
			t.Errorf("migrated object %s content differs on new primary (len got=%d want=%d)", o.key, len(got), len(o.body))
		}
		if rec := gwGet(t, hWasabi, bucket, o.key); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), o.body) {
			t.Errorf("post-migration GET %s status=%d correct=%v", o.key, rec.Code, bytes.Equal(rec.Body.Bytes(), o.body))
		}
	}
	// Concurrently-written objects must all be durably readable.
	for i, b := range newBodies {
		if rec := gwGet(t, hLocal, bucket, fmt.Sprintf("new-%d", i)); rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), b) {
			t.Errorf("concurrently-written object new-%d lost: status=%d correct=%v", i, rec.Code, bytes.Equal(rec.Body.Bytes(), b))
		}
	}
}

// failClosedAvailable reports whether Session 1's fail-closed
// budget-resolution gate (e.g. a RateLimiter.FailClosed knob that
// rejects requests whose egress budget cannot be resolved) has merged
// into the in-memory limiter. It is intentionally a const false on
// current main: the limiter fails OPEN when EgressLookup returns
// ok=false (see AllowEgress in internal/auth/rate_limit.go). Flip this
// to true once Session 1 lands so the unresolved-budget assertion
// below tightens from "fails open (today)" to "fails closed". The PR
// notes this cross-session dependency.
const failClosedAvailable = false

// TestChaos_RateLimiterFailClosed exercises the in-memory rate
// limiter's egress-budget resolution path. It pins down two
// behaviours:
//
//  1. A RESOLVED, exhausted egress budget fails closed today: once a
//     tenant has served its monthly quota the limiter denies further
//     egress. This is the guarantee that holds on current main.
//  2. A budget that cannot be RESOLVED (lookup ok=false) currently
//     fails OPEN. This is the path Session 1's FailClosed work
//     governs; the assertion is guarded by failClosedAvailable so the
//     test stays green on main and tightens automatically once
//     Session 1 merges.
func TestChaos_RateLimiterFailClosed(t *testing.T) {
	rpsLookup := func(string) (int, int, bool) { return 1000, 1000, true }
	resolver := func(*http.Request) (string, bool) { return "tenant-A", true }

	// (1) Resolved-but-exhausted budget must fail closed.
	rl := auth.NewRateLimiter(rpsLookup, resolver)
	rl.EgressLookup = func(string) (int64, bool) { return 8, true } // 8-byte monthly budget
	if !rl.AllowEgress("tenant-A") {
		t.Fatal("fresh tenant under an unspent budget must be allowed")
	}
	rl.Observe("tenant-A", 1024) // blow past the budget
	if rl.AllowEgress("tenant-A") {
		t.Fatal("exhausted egress budget must fail closed, but the limiter allowed the request")
	}

	// (2) Unresolvable budget: the FailClosed-governed path.
	rl2 := auth.NewRateLimiter(rpsLookup, resolver)
	rl2.EgressLookup = func(string) (int64, bool) { return 0, false } // cannot resolve the budget
	allowed := rl2.AllowEgress("tenant-A")
	if failClosedAvailable {
		if allowed {
			t.Fatal("with the FailClosed gate merged, an unresolvable egress budget must fail closed")
		}
	} else {
		if !allowed {
			t.Fatal("on current main an unresolvable egress budget fails OPEN; got a rejection — did Session 1 merge? flip failClosedAvailable")
		}
	}
}

// --- helpers ---

// chaosPlacement is a minimal s3compat.PlacementEngine that resolves
// every object to a fixed backend, mirroring fixedPlacement in the
// s3compat package tests. The single-backend AllowedBackends keeps the
// placement policy self-consistent for the gateway's validation.
type chaosPlacement struct{ backend string }

func (p chaosPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{AllowedBackends: []string{p.backend}}, nil
}

// newChaosBacking builds a real local_fs_dev provider rooted in the
// test's temp dir. Each call gets an isolated root so multiple
// backends in one test (e.g. a migration source and target) do not
// collide.
func newChaosBacking(t *testing.T) providers.StorageProvider {
	t.Helper()
	p, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	return p
}

// newChaosGateway constructs an s3compat handler with a deterministic
// clock (advancing one second per call so repeated PUTs of one key
// get distinct version ids, matching newAdvancingClockTestHandler in
// the s3compat tests).
func newChaosGateway(t *testing.T, cfg s3compat.Config) *s3compat.Handler {
	t.Helper()
	if cfg.Now == nil {
		now := time.Unix(1700000000, 0)
		var mu sync.Mutex
		cfg.Now = func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			cur := now
			now = now.Add(time.Second)
			return cur
		}
	}
	return s3compat.New(cfg)
}

func gwPut(t *testing.T, h *s3compat.Handler, bucket, key string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"/"+key, bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	return rec
}

func gwGet(t *testing.T, h *s3compat.Handler, bucket, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+bucket+"/"+key, nil)
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	return rec
}

// latestManifest returns a clone of the current manifest for
// bucket/key, looked up by the gateway's object-key hash. It fails the
// test if the object is absent.
func latestManifest(t *testing.T, store manifest_store.ManifestStore, bucket, key string) *metadata.ObjectManifest {
	t.Helper()
	mkey := manifest_store.ManifestKey{
		TenantID:      s3compat.AnonymousTenant,
		Bucket:        bucket,
		ObjectKeyHash: chaosObjectKeyHash(key),
	}
	m, err := store.Get(context.Background(), mkey)
	if err != nil {
		t.Fatalf("latestManifest %s/%s: %v", bucket, key, err)
	}
	return m
}

// chaosObjectKeyHash mirrors api/s3compat's unexported hashObjectKey
// (sha256 hex of the raw key) so a test can reconstruct the manifest
// key the gateway wrote under without reaching into the handler's
// internals.
func chaosObjectKeyHash(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

type fakeHealthSource struct {
	signal repair.HealthSignal
	err    error
}

func (f *fakeHealthSource) Poll(_ context.Context) (repair.HealthSignal, error) {
	return f.signal, f.err
}

type fakeManifestScanner struct {
	manifests []*metadata.ObjectManifest
}

func (f *fakeManifestScanner) FindManifestsByPieceID(_ context.Context, _ []string) ([]*metadata.ObjectManifest, error) {
	return f.manifests, nil
}


