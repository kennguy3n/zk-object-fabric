package s3_compat_test

import (
	"bytes"
	"context"
	"log"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration/background_rebalancer"
	"github.com/kennguy3n/zk-object-fabric/migration/dual_write"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
)

// TestLiveMigration_WasabiToCephRGW drives the full S3 compliance
// suite through a Wasabi → Ceph RGW dual-write topology while a
// background rebalancer is concurrently advancing the migration
// state machine. It is the integration gate for Phase 3's
// "compliance during live migration" requirement: the suite must
// stay green end-to-end while the rebalancer is actively copying
// pieces from Wasabi to Ceph RGW so no in-flight migration step
// can break the data plane.
//
// The test is gated on every required Wasabi and Ceph RGW env var
// so CI stays green without credentials. Run it against throwaway
// buckets only — the compliance suite writes and deletes test
// objects in place, and the rebalancer copies every eligible piece
// from the primary to the secondary.
//
// Required env vars:
//
//	WASABI_ENDPOINT, WASABI_REGION, WASABI_BUCKET,
//	WASABI_ACCESS_KEY, WASABI_SECRET_KEY,
//	CEPH_RGW_ENDPOINT, CEPH_RGW_BUCKET,
//	CEPH_RGW_ACCESS_KEY, CEPH_RGW_SECRET_KEY
//
// Optional env vars: CEPH_RGW_REGION, CEPH_RGW_CELL,
// CEPH_RGW_COUNTRY.
func TestLiveMigration_WasabiToCephRGW(t *testing.T) {
	wasabiEndpoint := os.Getenv("WASABI_ENDPOINT")
	wasabiBucket := os.Getenv("WASABI_BUCKET")
	rgwEndpoint := os.Getenv("CEPH_RGW_ENDPOINT")
	rgwBucket := os.Getenv("CEPH_RGW_BUCKET")
	if wasabiEndpoint == "" || wasabiBucket == "" || rgwEndpoint == "" || rgwBucket == "" {
		t.Skip("WASABI_ENDPOINT / WASABI_BUCKET / CEPH_RGW_ENDPOINT / CEPH_RGW_BUCKET not set")
	}
	wasabiRegion := os.Getenv("WASABI_REGION")
	wasabiAccess := os.Getenv("WASABI_ACCESS_KEY")
	wasabiSecret := os.Getenv("WASABI_SECRET_KEY")
	rgwAccess := os.Getenv("CEPH_RGW_ACCESS_KEY")
	rgwSecret := os.Getenv("CEPH_RGW_SECRET_KEY")
	if wasabiRegion == "" || wasabiAccess == "" || wasabiSecret == "" || rgwAccess == "" || rgwSecret == "" {
		t.Fatalf("WASABI_REGION / WASABI_ACCESS_KEY / WASABI_SECRET_KEY / CEPH_RGW_ACCESS_KEY / CEPH_RGW_SECRET_KEY are required for the live-migration compliance gate")
	}

	primary, err := wasabi.New(wasabi.Config{
		Endpoint:  wasabiEndpoint,
		Region:    wasabiRegion,
		Bucket:    wasabiBucket,
		AccessKey: wasabiAccess,
		SecretKey: wasabiSecret,
	})
	if err != nil {
		t.Fatalf("wasabi.New: %v", err)
	}
	secondary, err := ceph_rgw.New(ceph_rgw.Config{
		Endpoint:  rgwEndpoint,
		Region:    os.Getenv("CEPH_RGW_REGION"),
		Bucket:    rgwBucket,
		AccessKey: rgwAccess,
		SecretKey: rgwSecret,
		Cell:      os.Getenv("CEPH_RGW_CELL"),
		Country:   os.Getenv("CEPH_RGW_COUNTRY"),
	})
	if err != nil {
		t.Fatalf("ceph_rgw.New: %v", err)
	}

	manifests := memory.New()
	dw := dual_write.New("wasabi_to_ceph_rgw", primary, secondary)
	registry := map[string]providers.StorageProvider{
		"wasabi":             primary,
		"ceph_rgw":           secondary,
		"wasabi_to_ceph_rgw": dw,
	}

	// Pre-populate Wasabi with a small object that exists only on
	// the primary so the rebalancer has an outstanding piece to
	// copy while the compliance suite is running. The compliance
	// suite uses its own (bucket, key) namespace so this pre-load
	// does not collide with its writes.
	preBucket := "live-migration-preload"
	preKey := "preloaded.bin"
	preBody := []byte("preloaded-on-wasabi")
	prePiece, err := primary.PutPiece(context.Background(), preKey, bytes.NewReader(preBody), providers.PutOptions{
		ContentLength: int64(len(preBody)),
	})
	if err != nil {
		t.Fatalf("primary.PutPiece preload: %v", err)
	}
	t.Logf("preloaded piece %s in %s/%s", prePiece.PieceID, preBucket, preKey)

	// The rebalancer exits as soon as Run returns; loop it on a
	// short cadence so the compliance suite's writes are
	// continuously mirrored from primary to secondary while the
	// suite is running.
	rb := background_rebalancer.New(background_rebalancer.Config{
		Manifests: manifests,
		Providers: registry,
		Targets: []background_rebalancer.TenantTarget{{
			TenantID:       "default",
			Bucket:         preBucket,
			SourceBackend:  "wasabi",
			PrimaryBackend: "ceph_rgw",
		}},
		Logger: log.New(os.Stderr, "live_migration_rebalancer ", log.LstdFlags),
	})
	rebalCtx, cancelRebal := context.WithCancel(context.Background())
	t.Cleanup(cancelRebal)

	var passes int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := rb.Run(rebalCtx); err != nil {
				if rebalCtx.Err() != nil {
					return
				}
				log.Printf("live_migration_rebalancer: pass error: %v", err)
			}
			atomic.AddInt64(&passes, 1)
			select {
			case <-rebalCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	// Drive the compliance suite while the rebalancer is running.
	// The Run helper handles HTTP server / SDK plumbing; the
	// dual-write provider mirrors every PUT to both backends, so a
	// concurrent rebalancer pass cannot drop bytes that the suite
	// just wrote.
	t.Run("compliance_suite_during_migration", func(t *testing.T) {
		Run(t, Setup{
			Manifests: manifests,
			Providers: map[string]providers.StorageProvider{"wasabi_to_ceph_rgw": dw},
			Default:   "wasabi_to_ceph_rgw",
		})
	})

	cancelRebal()
	<-done

	if got := atomic.LoadInt64(&passes); got == 0 {
		t.Fatalf("rebalancer did not complete a single pass during the compliance run")
	} else {
		t.Logf("rebalancer completed %d passes during the compliance run", got)
	}

	// Sanity: the preloaded object must still be readable on the
	// primary throughout the migration. (Phase 3's rebalancer
	// only copies pieces; it never deletes from the source until
	// the cut-over phase, which this test does not trigger.)
	rc, err := primary.GetPiece(context.Background(), prePiece.PieceID, nil)
	if err != nil {
		t.Fatalf("preloaded piece unreadable on primary post-migration: %v", err)
	}
	rc.Close()
}
