package s3_compat_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
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
