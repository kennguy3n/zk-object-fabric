package chaos

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/repair"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
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
func TestChaos_FaultManifestStoreEventualHealReleasesBackpressure(t *testing.T) {
	inner := memory.New()
	fms := NewFaultManifestStore(inner)
	healAt := time.Now().Add(50 * time.Millisecond)
	fms.PutFault = FaultConfig{
		Mode:      ModeFailUntilTime,
		Err:       errors.New("chaos: pg failover in progress"),
		FailUntil: healAt,
	}

	key := manifest_store.ManifestKey{TenantID: "T", Bucket: "b", ObjectKeyHash: "k"}
	m := &metadata.ObjectManifest{TenantID: "T", Bucket: "b", ObjectKey: "k", ObjectKeyHash: "k"}

	// During the outage window every Put must fail.
	for i := 0; i < 5; i++ {
		if err := fms.Put(context.Background(), key, m); err == nil {
			t.Fatalf("Put attempt %d succeeded during outage window; "+
				"FailUntilTime must keep failing every call before "+
				"the deadline", i)
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Sleep past the heal point, then Put must succeed.
	time.Sleep(time.Until(healAt) + 10*time.Millisecond)
	if err := fms.Put(context.Background(), key, m); err != nil {
		t.Fatalf("Put after heal returned %v; want nil "+
			"(FailUntilTime should release at the deadline)", err)
	}
	// And the stored manifest must be retrievable.
	if got, err := inner.Get(context.Background(), key); err != nil || got == nil {
		t.Errorf("Get after heal: err=%v got=%v; want a stored manifest "+
			"— the post-heal Put must commit through the wrapper", err, got)
	}
}

// --- helpers ---

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


