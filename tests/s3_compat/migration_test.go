package s3_compat_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/migration/dual_write"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
)

// TestMigration_DualWriteSuite drives the full S3 compliance suite
// through a DualWriteProvider topology so the cut-over from backend
// A to backend B is verified to be behaviourally indistinguishable
// from a single-backend configuration.
func TestMigration_DualWriteSuite(t *testing.T) {
	primary, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev primary: %v", err)
	}
	secondary, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev secondary: %v", err)
	}
	dw := dual_write.New("dual_write", primary, secondary)

	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"dual_write": dw},
		Default:   "dual_write",
	})
}

// TestMigration_WritesAreMirrored asserts that every PUT served
// through a DualWriteProvider lands on both backends, which is the
// invariant the background rebalancer relies on when it advances
// the migration state machine.
func TestMigration_WritesAreMirrored(t *testing.T) {
	primary, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev primary: %v", err)
	}
	secondary, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev secondary: %v", err)
	}
	dw := dual_write.New("dual_write", primary, secondary)

	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"dual_write": dw},
		Placement: fixedPlacement{backend: "dual_write"},
		Now:       time.Now,
	}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("load sdk config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})

	key := "mirror.bin"
	body := []byte("dual-write content")
	if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("b"),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Under the piece-id hashing scheme the handler uses, both
	// backends must see exactly one piece. We verify by listing each
	// backend directly.
	primaryList, err := primary.ListPieces(context.Background(), "", "")
	if err != nil {
		t.Fatalf("primary.ListPieces: %v", err)
	}
	secondaryList, err := secondary.ListPieces(context.Background(), "", "")
	if err != nil {
		t.Fatalf("secondary.ListPieces: %v", err)
	}
	if len(primaryList.Pieces) != 1 {
		t.Fatalf("primary pieces = %d, want 1", len(primaryList.Pieces))
	}
	if len(secondaryList.Pieces) != 1 {
		t.Fatalf("secondary pieces = %d, want 1 (dual-write did not mirror)", len(secondaryList.Pieces))
	}
	if primaryList.Pieces[0].PieceID != secondaryList.Pieces[0].PieceID {
		t.Fatalf("piece_id mismatch across backends: primary=%q secondary=%q",
			primaryList.Pieces[0].PieceID, secondaryList.Pieces[0].PieceID)
	}

	// And read-back must still return the correct bytes after a
	// primary-side failure — drop the piece from the primary and
	// verify the secondary fallback path serves the object.
	if err := primary.DeletePiece(context.Background(), primaryList.Pieces[0].PieceID); err != nil {
		t.Fatalf("primary.DeletePiece: %v", err)
	}
	out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String("b"),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after primary piece drop: %v", err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read fallback body: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("fallback body mismatch: got %q want %q", data, body)
	}
}

// TestMigration_WasabiToCephRGW runs the full compliance suite
// through a DualWriteProvider that mirrors every write from Wasabi
// (primary) to Ceph RGW (secondary). It is the integration gate
// for the Phase 3 Ceph-RGW cell bring-up: the suite must pass
// end-to-end against the dual-write topology before the rebalancer
// is allowed to advance a tenant from state=mirror to
// state=cutover.
//
// The test is gated on CEPH_RGW_ENDPOINT to keep it out of the
// default CI path. A local Ceph RGW reachable at that endpoint plus
// Wasabi-compatible credentials are required:
//
//	CEPH_RGW_ENDPOINT, CEPH_RGW_BUCKET,
//	CEPH_RGW_ACCESS_KEY, CEPH_RGW_SECRET_KEY,
//	CEPH_RGW_REGION, CEPH_RGW_CELL, CEPH_RGW_COUNTRY
//	WASABI_ENDPOINT, WASABI_REGION, WASABI_BUCKET,
//	WASABI_ACCESS_KEY, WASABI_SECRET_KEY
//
// Run this against throwaway buckets only: the compliance suite
// writes and deletes test objects in place.
func TestMigration_WasabiToCephRGW(t *testing.T) {
	rgwEndpoint := os.Getenv("CEPH_RGW_ENDPOINT")
	if rgwEndpoint == "" {
		t.Skip("CEPH_RGW_ENDPOINT not set")
	}
	rgwBucket := os.Getenv("CEPH_RGW_BUCKET")
	rgwAccess := os.Getenv("CEPH_RGW_ACCESS_KEY")
	rgwSecret := os.Getenv("CEPH_RGW_SECRET_KEY")
	if rgwBucket == "" || rgwAccess == "" || rgwSecret == "" {
		t.Fatalf("CEPH_RGW_ENDPOINT is set but CEPH_RGW_BUCKET / CEPH_RGW_ACCESS_KEY / CEPH_RGW_SECRET_KEY are missing")
	}
	wasabiEndpoint := os.Getenv("WASABI_ENDPOINT")
	wasabiRegion := os.Getenv("WASABI_REGION")
	wasabiBucket := os.Getenv("WASABI_BUCKET")
	wasabiAccess := os.Getenv("WASABI_ACCESS_KEY")
	wasabiSecret := os.Getenv("WASABI_SECRET_KEY")
	if wasabiEndpoint == "" || wasabiRegion == "" || wasabiBucket == "" || wasabiAccess == "" || wasabiSecret == "" {
		t.Fatalf("WASABI_ENDPOINT / WASABI_REGION / WASABI_BUCKET / WASABI_ACCESS_KEY / WASABI_SECRET_KEY are required for the Wasabi→Ceph RGW migration gate")
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
	dw := dual_write.New("wasabi_to_ceph_rgw", primary, secondary)

	// First: drive the full compliance suite through the dual-write
	// topology so every PUT / GET / HEAD / LIST / DELETE / range /
	// multipart path is exercised against both backends.
	t.Run("compliance_suite", func(t *testing.T) {
		Run(t, Setup{
			Manifests: memory.New(),
			Providers: map[string]providers.StorageProvider{"wasabi_to_ceph_rgw": dw},
			Default:   "wasabi_to_ceph_rgw",
		})
	})

	// Second: verify the mirror + fallback invariants directly so a
	// broken background rebalancer cannot hide behind a green
	// compliance run. We PUT one object, assert it landed on both
	// backends, drop it from the primary, and then GET through the
	// gateway to confirm the dual-write fallback served it.
	t.Run("mirror_and_fallback", func(t *testing.T) {
		mux := http.NewServeMux()
		s3compat.New(s3compat.Config{
			Manifests: memory.New(),
			Providers: map[string]providers.StorageProvider{"wasabi_to_ceph_rgw": dw},
			Placement: fixedPlacement{backend: "wasabi_to_ceph_rgw"},
			Now:       time.Now,
		}).Register(mux)
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)

		cfg, err := config.LoadDefaultConfig(
			context.Background(),
			config.WithRegion("us-east-1"),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		)
		if err != nil {
			t.Fatalf("load sdk config: %v", err)
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ts.URL)
			o.UsePathStyle = true
		})

		key := "rebalancer-gate.bin"
		body := []byte("wasabi→ceph_rgw dual-write content")
		if _, err := client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String("b"),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("PutObject: %v", err)
		}

		primaryList, err := primary.ListPieces(context.Background(), "", "")
		if err != nil {
			t.Fatalf("primary.ListPieces: %v", err)
		}
		secondaryList, err := secondary.ListPieces(context.Background(), "", "")
		if err != nil {
			t.Fatalf("secondary.ListPieces: %v", err)
		}
		if len(primaryList.Pieces) == 0 {
			t.Fatalf("primary did not receive mirrored piece")
		}
		if len(secondaryList.Pieces) == 0 {
			t.Fatalf("secondary (Ceph RGW) did not receive mirrored piece")
		}

		// Drop the piece from the primary and verify the gateway
		// transparently falls back to Ceph RGW.
		if err := primary.DeletePiece(context.Background(), primaryList.Pieces[0].PieceID); err != nil {
			t.Fatalf("primary.DeletePiece: %v", err)
		}
		out, err := client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String("b"),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject after primary piece drop: %v", err)
		}
		defer out.Body.Close()
		data, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("read fallback body: %v", err)
		}
		if !bytes.Equal(data, body) {
			t.Fatalf("fallback body mismatch: got %q want %q", data, body)
		}
	})
}
