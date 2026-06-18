// End-to-end S3-compliance tests for intra-tenant
// deduplication. The tests cover:
//
//   - Pattern B: managed encryption + dedup on. Two PUTs with
//     identical plaintext produce a single backend piece.
//   - Pattern C: client_side encryption + dedup on. Two PUTs with
//     byte-identical convergent ciphertext produce a single
//     backend piece.
//   - Reference-counted DELETE: the first DELETE leaves the
//     backend piece intact and lets the second copy still GET; the
//     second DELETE removes the piece.
//   - Multipart dedup (single piece): two multipart uploads with
//     the same content land on a single backend piece.
//
// All tests run against the local_fs_dev provider so the harness can
// assert directly against the on-disk piece set.

package s3_compat_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// dedupPlacement is a PlacementEngine that always resolves to one
// backend and stamps every object with a DedupPolicy{Enabled:true}.
// Tests that exercise the "dedup off" path use the standard
// fixedPlacement instead.
type dedupPlacement struct {
	backend        string
	encryptionMode string
}

func (p dedupPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		EncryptionMode:  p.encryptionMode,
		DedupPolicy: &metadata.DedupPolicy{
			Enabled: true,
			Scope:   "intra_tenant",
			Level:   "object",
		},
	}, nil
}

// dedupServer is the dedup-aware test harness. It bundles the same
// pieces encryptionServer does plus the in-memory ContentIndex so
// tests can assert refcount transitions.
type dedupServer struct {
	ts           *httptest.Server
	client       *s3.Client
	bucket       string
	pieceRoot    string
	contentIndex *content_index.MemoryStore
}

func newDedupServer(t *testing.T, encMode string) *dedupServer {
	t.Helper()
	pieceRoot := t.TempDir()
	backend, err := local_fs_dev.New(pieceRoot)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}

	var gatewayEnc *s3compat.GatewayEncryption
	if encMode == "managed" || encMode == "public_distribution" {
		cmkPath := filepath.Join(t.TempDir(), "cmk.key")
		cmkMaterial := make([]byte, chacha20poly1305.KeySize)
		if _, err := rand.Read(cmkMaterial); err != nil {
			t.Fatalf("rand cmk: %v", err)
		}
		if err := os.WriteFile(cmkPath, cmkMaterial, 0o600); err != nil {
			t.Fatalf("write cmk: %v", err)
		}
		gatewayEnc = &s3compat.GatewayEncryption{
			Wrapper: client_sdk.LocalFileWrapper{Path: cmkPath},
			CMK: encryption.CustomerMasterKeyRef{
				URI:         "cmk://test/dedup",
				Version:     1,
				HolderClass: "gateway_hsm",
			},
		}
	}

	cidx := content_index.NewMemoryStore()
	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests:     memory.New(),
		Providers:     map[string]providers.StorageProvider{"local_fs_dev": backend},
		Placement:     dedupPlacement{backend: "local_fs_dev", encryptionMode: encMode},
		Multipart:     multipart.NewMemoryStore(),
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Encryption:    gatewayEnc,
		ContentIndex:  cidx,
		Now:           time.Now,
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
	return &dedupServer{
		ts:           ts,
		client:       client,
		bucket:       "dedup-bucket",
		pieceRoot:    pieceRoot,
		contentIndex: cidx,
	}
}

// countPieces returns the number of *.bin files under the local_fs_dev
// piece root. Tests use it to verify that a hit-path PUT did NOT
// add a new physical piece.
func (s *dedupServer) countPieces(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(s.pieceRoot)
	if err != nil {
		t.Fatalf("read pieceRoot: %v", err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".bin") {
			n++
		}
	}
	return n
}

func TestDedup_PatternB_PutTwiceShareSinglePiece(t *testing.T) {
	s := newDedupServer(t, "managed")
	body := bytes.Repeat([]byte("dedup-pattern-b-payload-"), 64)

	for _, key := range []string{"a.txt", "b.txt"} {
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}

	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected 1 backend piece after dedup, got %d", got)
	}
	for _, key := range []string{"a.txt", "b.txt"} {
		out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject %s: %v", key, err)
		}
		data, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !bytes.Equal(data, body) {
			t.Fatalf("GetObject %s: roundtrip mismatch", key)
		}
	}
}

func TestDedup_PatternB_DeleteRefcounts(t *testing.T) {
	s := newDedupServer(t, "managed")
	body := []byte("dedup-delete-refcount-payload-xyz")

	for _, key := range []string{"a.txt", "b.txt"} {
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected 1 piece after two PUTs, got %d", got)
	}

	// Delete the first copy. The piece must remain because the
	// second copy still references it.
	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("a.txt"),
	}); err != nil {
		t.Fatalf("DeleteObject a: %v", err)
	}
	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected piece to survive first DELETE, got %d", got)
	}
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.txt"),
	})
	if err != nil {
		t.Fatalf("GetObject b after delete a: %v", err)
	}
	data, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if !bytes.Equal(data, body) {
		t.Fatalf("GetObject b roundtrip mismatch after delete a")
	}

	// Delete the second copy: the refcount drops to zero, the
	// backend piece is removed, and content_index has the row
	// gone.
	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.txt"),
	}); err != nil {
		t.Fatalf("DeleteObject b: %v", err)
	}
	if got := s.countPieces(t); got != 0 {
		t.Fatalf("expected piece removed after final DELETE, got %d", got)
	}
}

func TestDedup_PatternC_ConvergentClientCiphertext(t *testing.T) {
	s := newDedupServer(t, "client_side")
	// Pattern C requires the client to send byte-identical
	// ciphertext for two uploads. We emulate that by using the
	// gateway's own client SDK with a tenant-shared, content-
	// derived DEK and the convergent-nonce option, then pre-
	// reading the resulting ciphertext into a buffer that both
	// PUTs reuse. The gateway never sees plaintext; it dedups on
	// BLAKE3(ciphertext).
	plaintext := []byte("client-side-convergent-payload-stable")
	hash := []byte("00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff")
	dek, err := client_sdk.DeriveConvergentDEK(hash, "tnt")
	if err != nil {
		t.Fatalf("DeriveConvergentDEK: %v", err)
	}
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{ConvergentNonce: true})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	for _, key := range []string{"a.bin", "b.bin"} {
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(ciphertext),
			Metadata: map[string]string{
				"Zk-Encryption": "client_side",
			},
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected 1 backend piece for convergent client ciphertext, got %d", got)
	}
}

func TestDedup_Multipart_SinglePartShareSinglePiece(t *testing.T) {
	// Multipart dedup is asserted in client_side mode: the
	// client sends byte-identical bodies for both uploads so the
	// gateway-computed BLAKE3 over the assembled pieces matches.
	// Managed multipart per-upload DEKs make convergent
	// ciphertext impossible — that path is covered by single-PUT
	// Pattern B and by the "object+block" tier which lives in
	// the §3.14 Ceph RGW mode (out of scope for object-level
	// dedup).
	// Use the no-encryption placement so the test does not have
	// to weave the X-Amz-Meta-Zk-Encryption header through every
	// UploadPart call. The dedup pipeline runs identically — it
	// hashes the assembled piece bytes regardless of whether the
	// gateway encrypted them — so this still exercises the
	// content_index lookup / register / decrement path.
	s := newDedupServer(t, "")
	// Single-part multipart upload — the per-part minimum size
	// applies only when N>1 parts are uploaded, so we send one
	// 4 KiB part to keep the test fast.
	body := bytes.Repeat([]byte("multipart-dedup-payload-"), 4*1024/24+1)
	body = body[:4*1024]

	for _, key := range []string{"a.bin", "b.bin"} {
		create, err := s.client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key),
		})
		if err != nil {
			t.Fatalf("CreateMultipartUpload %s: %v", key, err)
		}
		part, err := s.client.UploadPart(context.Background(), &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   create.UploadId,
			PartNumber: aws.Int32(1),
			Body:       bytes.NewReader(body),
		})
		if err != nil {
			t.Fatalf("UploadPart %s: %v", key, err)
		}
		if _, err := s.client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: create.UploadId,
			MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{
				{ETag: part.ETag, PartNumber: aws.Int32(1)},
			}},
		}); err != nil {
			t.Fatalf("CompleteMultipartUpload %s: %v", key, err)
		}
	}

	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected 1 backend piece after multipart dedup, got %d", got)
	}
}

// uploadMultiPartMultipart drives N-part multipart uploads with the
// supplied per-part bodies for both target keys. It is the shared
// driver for the multi-piece multipart dedup tests; the encryption
// header is opt-in so the same helper covers the unencrypted ("")
// mode used by TestDedup_Multipart_MultiPart_* tests and any future
// client_side scenarios that wire up the convergent SDK.
func uploadMultiPartMultipart(t *testing.T, s *dedupServer, key string, parts [][]byte, encHeader string) {
	t.Helper()
	ctx := context.Background()
	createIn := &s3.CreateMultipartUploadInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if encHeader != "" {
		createIn.Metadata = map[string]string{"Zk-Encryption": encHeader}
	}
	create, err := s.client.CreateMultipartUpload(ctx, createIn)
	if err != nil {
		t.Fatalf("CreateMultipartUpload %s: %v", key, err)
	}
	completed := make([]s3types.CompletedPart, 0, len(parts))
	for i, body := range parts {
		partNumber := int32(i + 1)
		// UploadPartInput doesn't accept user metadata —
		// CreateMultipartUpload's metadata is the only place
		// the gateway reads the X-Amz-Meta-Zk-Encryption hint
		// from. Per-part encryption header validation runs
		// against the upload session's EncMode, which was
		// captured at Create time, so this is sufficient.
		uploadIn := &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   create.UploadId,
			PartNumber: aws.Int32(partNumber),
			Body:       bytes.NewReader(body),
		}
		out, err := s.client.UploadPart(ctx, uploadIn)
		if err != nil {
			t.Fatalf("UploadPart %s part %d: %v", key, partNumber, err)
		}
		completed = append(completed, s3types.CompletedPart{ETag: out.ETag, PartNumber: aws.Int32(partNumber)})
	}
	if _, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(s.bucket), Key: aws.String(key), UploadId: create.UploadId,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: completed},
	}); err != nil {
		t.Fatalf("CompleteMultipartUpload %s: %v", key, err)
	}
}

// TestDedup_Multipart_MultiPartShareSingleCanonical asserts the
// multi-piece dedup happy path: two multipart uploads of the same
// 3-part body in unencrypted mode dedup to a single canonical
// piece set, so the backend holds 3 pieces (one per part), not 6.
// Both manifest GETs return the original concatenated bytes.
func TestDedup_Multipart_MultiPartShareSingleCanonical(t *testing.T) {
	s := newDedupServer(t, "")
	parts := [][]byte{
		bytes.Repeat([]byte("multi-piece-part-1-payload-"), 256),
		bytes.Repeat([]byte("multi-piece-part-2-payload-"), 256),
		bytes.Repeat([]byte("multi-piece-part-3-payload-"), 64),
	}
	want := bytes.Join(parts, nil)

	uploadMultiPartMultipart(t, s, "a.bin", parts, "")
	uploadMultiPartMultipart(t, s, "b.bin", parts, "")

	if got := s.countPieces(t); got != len(parts) {
		t.Fatalf("expected %d backend pieces after multi-piece dedup, got %d", len(parts), got)
	}
	for _, key := range []string{"a.bin", "b.bin"} {
		out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject %s: %v", key, err)
		}
		got, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("GetObject %s: %d bytes returned, want %d (multi-piece roundtrip mismatch)",
				key, len(got), len(want))
		}
	}
}

// TestDedup_Multipart_MultiPart_ManagedConsolidates asserts the
// deferred convergent consolidation path: two multipart uploads of
// the same plaintext under `managed` encryption produce exactly
// one backend piece. The first upload's per-part pieces (sealed
// with the session's random DEK) are decrypted, the assembled
// plaintext is re-encrypted with a convergent DEK derived from
// the per-part plaintext digests, and the consolidated piece is
// registered against the canonical plaintext_hash. The second
// upload looks up the same plaintext_hash, increments the refcount
// on the existing consolidated piece, and drops every newly
// uploaded part.
//
// Both objects must roundtrip through GET — the manifests carry
// independently wrapped copies of the same convergent DEK so the
// gateway can decrypt the canonical piece for either reader.
func TestDedup_Multipart_MultiPart_ManagedConsolidates(t *testing.T) {
	s := newDedupServer(t, "managed")
	parts := [][]byte{
		bytes.Repeat([]byte("managed-multipart-part-1-"), 256),
		bytes.Repeat([]byte("managed-multipart-part-2-"), 256),
	}
	want := bytes.Join(parts, nil)

	uploadMultiPartMultipart(t, s, "a.bin", parts, "")
	uploadMultiPartMultipart(t, s, "b.bin", parts, "")

	if got := s.countPieces(t); got != 1 {
		t.Fatalf("managed multipart must dedup to one consolidated piece, got %d", got)
	}
	for _, key := range []string{"a.bin", "b.bin"} {
		out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucket), Key: aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject %s: %v", key, err)
		}
		got, _ := io.ReadAll(out.Body)
		_ = out.Body.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("GetObject %s: roundtrip mismatch after managed multipart dedup", key)
		}
	}
}

// TestDedup_Multipart_MultiPart_ManagedDeleteRefcount asserts the
// refcount contract for deferred convergent consolidation: two
// uploads share one consolidated piece, the first DELETE leaves
// it intact (refcount 2→1), and the second DELETE reclaims it
// (refcount 1→0).
func TestDedup_Multipart_MultiPart_ManagedDeleteRefcount(t *testing.T) {
	s := newDedupServer(t, "managed")
	parts := [][]byte{
		bytes.Repeat([]byte("managed-multipart-refcount-1-"), 128),
		bytes.Repeat([]byte("managed-multipart-refcount-2-"), 128),
		bytes.Repeat([]byte("managed-multipart-refcount-3-"), 32),
	}
	want := bytes.Join(parts, nil)

	uploadMultiPartMultipart(t, s, "a.bin", parts, "")
	uploadMultiPartMultipart(t, s, "b.bin", parts, "")
	if got := s.countPieces(t); got != 1 {
		t.Fatalf("expected 1 consolidated piece after dedup, got %d", got)
	}

	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("a.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject a: %v", err)
	}
	if got := s.countPieces(t); got != 1 {
		t.Fatalf("consolidated piece must survive first DELETE, got %d", got)
	}
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject b after delete a: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("GetObject b roundtrip mismatch after delete a")
	}

	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject b: %v", err)
	}
	if got := s.countPieces(t); got != 0 {
		t.Fatalf("expected consolidated piece reclaimed after final DELETE, got %d", got)
	}
}

// TestDedup_Multipart_MultiPart_DeleteRefcount asserts that the
// refcounted-DELETE contract holds for multi-piece dedup: two
// uploads of the same content share one canonical piece set;
// deleting the first object leaves all canonical pieces intact
// (refcount drops 2→1) and the second upload still GETs
// successfully; deleting the second object reclaims every
// canonical piece (refcount 1→0).
func TestDedup_Multipart_MultiPart_DeleteRefcount(t *testing.T) {
	s := newDedupServer(t, "")
	parts := [][]byte{
		bytes.Repeat([]byte("multi-refcount-part-1-"), 128),
		bytes.Repeat([]byte("multi-refcount-part-2-"), 128),
		bytes.Repeat([]byte("multi-refcount-part-3-"), 32),
	}
	want := bytes.Join(parts, nil)

	uploadMultiPartMultipart(t, s, "a.bin", parts, "")
	uploadMultiPartMultipart(t, s, "b.bin", parts, "")
	if got := s.countPieces(t); got != len(parts) {
		t.Fatalf("expected %d pieces after dedup, got %d", len(parts), got)
	}

	// First DELETE: refcount drops to 1; pieces survive.
	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("a.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject a: %v", err)
	}
	if got := s.countPieces(t); got != len(parts) {
		t.Fatalf("pieces must survive first DELETE, got %d want %d", got, len(parts))
	}
	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.bin"),
	})
	if err != nil {
		t.Fatalf("GetObject b after delete a: %v", err)
	}
	got, _ := io.ReadAll(out.Body)
	_ = out.Body.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("GetObject b roundtrip mismatch after delete a")
	}

	// Second DELETE: refcount drops to 0; every canonical piece
	// is reclaimed in the same DELETE handler call.
	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket), Key: aws.String("b.bin"),
	}); err != nil {
		t.Fatalf("DeleteObject b: %v", err)
	}
	if got := s.countPieces(t); got != 0 {
		t.Fatalf("expected all pieces removed after final DELETE, got %d", got)
	}
}
