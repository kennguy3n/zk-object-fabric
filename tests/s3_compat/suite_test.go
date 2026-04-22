// Package s3_compat_test is Workstream 7: the end-to-end S3 compliance
// test suite. It runs the full PUT/GET/HEAD/DELETE/LIST/range operation
// set against the gateway's HTTP handler, wired to different
// StorageProvider backends, using the AWS SDK v2 as the client so the
// assertions reflect real-world SDK compatibility rather than raw
// HTTP canned requests.
//
// The same Run function is also invoked from migration_test.go with a
// DualWriteProvider topology so the suite doubles as the zero-downtime
// migration gate: every S3 operation must behave identically while a
// tenant is cutting over from backend A to backend B.
package s3_compat_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/aws_s3"
	"github.com/kennguy3n/zk-object-fabric/providers/backblaze_b2"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/cloudflare_r2"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// fixedPlacement resolves every object to a single fixed backend so
// the suite can stress each adapter in isolation.
type fixedPlacement struct{ backend string }

func (f fixedPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return f.backend, metadata.PlacementPolicy{AllowedBackends: []string{f.backend}}, nil
}

// Setup is the harness the suite uses to spin up one gateway
// instance. Tests compose it from a ManifestStore plus a named
// StorageProvider registry, then Run exercises the full S3 surface.
type Setup struct {
	Manifests manifest_store.ManifestStore
	Providers map[string]providers.StorageProvider
	Default   string
}

// server stands up the gateway handler behind an httptest.Server and
// returns an SDK client configured for path-style addressing.
type server struct {
	ts     *httptest.Server
	client *s3.Client
	bucket string
}

func newServer(t *testing.T, setup Setup) *server {
	t.Helper()
	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests: setup.Manifests,
		Providers: setup.Providers,
		Placement: fixedPlacement{backend: setup.Default},
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
	return &server{ts: ts, client: client, bucket: "compat-bucket"}
}

// Run exercises the full S3 operation matrix. It is exported from the
// test package via TestSuite_* entrypoints so each provider backend
// can be driven independently.
func Run(t *testing.T, setup Setup) {
	t.Helper()
	t.Run("PutGetHeadDelete", func(t *testing.T) { testPutGetHeadDelete(t, setup) })
	t.Run("RangeGet", func(t *testing.T) { testRangeGet(t, setup) })
	t.Run("ListObjects", func(t *testing.T) { testListObjects(t, setup) })
	t.Run("DeleteIsIdempotent", func(t *testing.T) { testDeleteIdempotent(t, setup) })
	t.Run("MissingKeyReturns404", func(t *testing.T) { testMissingKey(t, setup) })
	t.Run("PresignedGet", func(t *testing.T) { testPresignedGet(t, setup) })
	t.Run("MultipartLikeOverwrite", func(t *testing.T) { testMultipartLikeOverwrite(t, setup) })
}

func testPutGetHeadDelete(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	key := "hello.txt"
	body := []byte("zk-object-fabric s3 compliance")

	_, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	head, err := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("HeadObject: %v", err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(body)) {
		t.Fatalf("HeadObject ContentLength = %d, want %d", aws.ToInt64(head.ContentLength), len(body))
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer got.Body.Close()
	data, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("GetObject body mismatch: got %q want %q", data, body)
	}

	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	_, err = s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("HeadObject after delete: want error, got nil")
	}
}

func testRangeGet(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	key := "range.bin"
	body := []byte("0123456789ABCDEF")

	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	cases := []struct {
		name  string
		rng   string
		want  string
		wantN int64
	}{
		{"prefix", "bytes=0-4", "01234", 5},
		{"middle", "bytes=4-9", "456789", 6},
		{"tail", "bytes=10-", "ABCDEF", 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
				Range:  aws.String(tc.rng),
			})
			if err != nil {
				t.Fatalf("GetObject range=%s: %v", tc.rng, err)
			}
			defer out.Body.Close()
			data, err := io.ReadAll(out.Body)
			if err != nil {
				t.Fatalf("read range %s: %v", tc.rng, err)
			}
			if string(data) != tc.want {
				t.Fatalf("range %s body = %q, want %q", tc.rng, data, tc.want)
			}
			if aws.ToInt64(out.ContentLength) != tc.wantN {
				t.Fatalf("range %s ContentLength = %d, want %d", tc.rng, aws.ToInt64(out.ContentLength), tc.wantN)
			}
		})
	}
}

func testListObjects(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	keys := []string{"list/a", "list/b", "list/nested/c", "other"}
	for _, k := range keys {
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(k),
			Body:   bytes.NewReader([]byte(k)),
		}); err != nil {
			t.Fatalf("PutObject %q: %v", k, err)
		}
	}

	out, err := s.client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	seen := map[string]int64{}
	for _, o := range out.Contents {
		seen[aws.ToString(o.Key)] = aws.ToInt64(o.Size)
	}
	for _, k := range keys {
		got, ok := seen[k]
		if !ok {
			t.Errorf("LIST missing key %q (got %v)", k, seen)
			continue
		}
		if got != int64(len(k)) {
			t.Errorf("LIST size for %q = %d, want %d", k, got, len(k))
		}
	}
}

func testDeleteIdempotent(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	if _, err := s.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String("never-existed"),
	}); err != nil {
		t.Fatalf("DeleteObject of missing key: %v", err)
	}
}

func testMissingKey(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	_, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String("nope"),
	})
	if err == nil {
		t.Fatal("GetObject missing: want error, got nil")
	}
	var httpErr *smithyhttp.ResponseError
	if !errors.As(err, &httpErr) {
		t.Fatalf("GetObject missing: want ResponseError, got %T: %v", err, err)
	}
	if httpErr.HTTPStatusCode() != http.StatusNotFound {
		t.Fatalf("GetObject missing status = %d, want 404", httpErr.HTTPStatusCode())
	}
}

func testPresignedGet(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	key := "presign.txt"
	body := []byte("presigned body")
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	presigner := s3.NewPresignClient(s.client)
	req, err := presigner.PresignGetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(time.Minute))
	if err != nil {
		t.Fatalf("PresignGetObject: %v", err)
	}
	// Phase 2 does not yet enforce presigned-URL signatures, but the
	// URL must be usable: it must target the gateway and return the
	// object body over plain HTTP GET.
	if !strings.HasPrefix(req.URL, s.ts.URL) {
		t.Fatalf("presigned URL %q does not target test server %q", req.URL, s.ts.URL)
	}
	if _, err := url.Parse(req.URL); err != nil {
		t.Fatalf("presigned URL not parseable: %v", err)
	}
	resp, err := http.Get(req.URL)
	if err != nil {
		t.Fatalf("GET presigned URL: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("presigned GET status = %d, want 200", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read presigned body: %v", err)
	}
	if !bytes.Equal(data, body) {
		t.Fatalf("presigned body = %q, want %q", data, body)
	}
}

// testMultipartLikeOverwrite stands in for the multipart suite until
// the gateway ships a native CreateMultipartUpload path: we PUT the
// same key twice with different bodies and assert the second write
// fully supersedes the first, which is the behaviour the CompleteMPU
// step must eventually emulate.
func testMultipartLikeOverwrite(t *testing.T, setup Setup) {
	s := newServer(t, setup)
	key := "overwrite.bin"
	first := bytes.Repeat([]byte("A"), 16)
	second := bytes.Repeat([]byte("B"), 32)

	for i, body := range [][]byte{first, second} {
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(body),
		}); err != nil {
			t.Fatalf("PutObject[%d]: %v", i, err)
		}
	}

	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject after overwrite: %v", err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(data, second) {
		t.Fatalf("overwrite GET body mismatch: got %d bytes, want %d", len(data), len(second))
	}
}

// errorsAs is a tiny helper that forwards to errors.As. Using it
// keeps the test file free of stringly-typed error inspection while
// avoiding a bare `errors` import that would overlap with the
// standard-library name in places.
func errorsAs(err error, target any) bool {
	for err != nil {
		if u, ok := target.(**smithyhttp.ResponseError); ok {
			if re, match := err.(*smithyhttp.ResponseError); match {
				*u = re
				return true
			}
		}
		w, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = w.Unwrap()
	}
	return false
}

// TestSuite_LocalFSDev runs the full suite against the local_fs_dev
// adapter. This is the canonical developer loopback target.
func TestSuite_LocalFSDev(t *testing.T) {
	p, err := local_fs_dev.New(t.TempDir())
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"local_fs_dev": p},
		Default:   "local_fs_dev",
	})
}

// TestSuite_CephRGW runs the full S3 compliance suite against a live
// Ceph RADOS Gateway deployment. It is the Phase 3 gate required
// before production traffic can be cut over to a local-DC Ceph cell
// (see docs/PROGRESS.md Phase 3 checklist).
//
// The test is gated behind environment variables so CI does not need
// a Ceph cluster to run the unit test battery:
//
//	CEPH_RGW_ENDPOINT   — required, full RGW base URL
//	CEPH_RGW_BUCKET     — required, pre-created bucket
//	CEPH_RGW_ACCESS_KEY — required
//	CEPH_RGW_SECRET_KEY — required
//	CEPH_RGW_REGION     — optional, defaults to the provider default
//	CEPH_RGW_CELL       — optional, operator-assigned cell label
//	CEPH_RGW_COUNTRY    — optional, ISO-3166 alpha-2 code
//
// When CEPH_RGW_ENDPOINT is unset the test is skipped. Operators
// running this locally should point it at a throwaway bucket — the
// suite writes and deletes test objects in place.
func TestSuite_CephRGW(t *testing.T) {
	endpoint := os.Getenv("CEPH_RGW_ENDPOINT")
	if endpoint == "" {
		t.Skip("CEPH_RGW_ENDPOINT not set")
	}
	bucket := os.Getenv("CEPH_RGW_BUCKET")
	accessKey := os.Getenv("CEPH_RGW_ACCESS_KEY")
	secretKey := os.Getenv("CEPH_RGW_SECRET_KEY")
	if bucket == "" || accessKey == "" || secretKey == "" {
		t.Fatalf("CEPH_RGW_ENDPOINT is set but CEPH_RGW_BUCKET / CEPH_RGW_ACCESS_KEY / CEPH_RGW_SECRET_KEY are missing")
	}
	p, err := ceph_rgw.New(ceph_rgw.Config{
		Endpoint:  endpoint,
		Region:    os.Getenv("CEPH_RGW_REGION"),
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
		Cell:      os.Getenv("CEPH_RGW_CELL"),
		Country:   os.Getenv("CEPH_RGW_COUNTRY"),
	})
	if err != nil {
		t.Fatalf("ceph_rgw.New: %v", err)
	}
	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"ceph_rgw": p},
		Default:   "ceph_rgw",
	})
}

// TestSuite_BackblazeB2 runs the full S3 compliance suite against a
// Backblaze B2 S3-compatible endpoint.
//
//	B2_ENDPOINT    — required, e.g. https://s3.us-west-002.backblazeb2.com
//	B2_REGION      — required, e.g. us-west-002
//	B2_BUCKET      — required, throwaway bucket; suite writes/deletes in place
//	B2_ACCESS_KEY  — required
//	B2_SECRET_KEY  — required
//
// The test is skipped when B2_ENDPOINT is unset so CI does not need
// B2 credentials.
func TestSuite_BackblazeB2(t *testing.T) {
	endpoint := os.Getenv("B2_ENDPOINT")
	if endpoint == "" {
		t.Skip("B2_ENDPOINT not set")
	}
	region := os.Getenv("B2_REGION")
	bucket := os.Getenv("B2_BUCKET")
	accessKey := os.Getenv("B2_ACCESS_KEY")
	secretKey := os.Getenv("B2_SECRET_KEY")
	if region == "" || bucket == "" || accessKey == "" || secretKey == "" {
		t.Fatalf("B2_ENDPOINT is set but B2_REGION / B2_BUCKET / B2_ACCESS_KEY / B2_SECRET_KEY are missing")
	}
	p, err := backblaze_b2.New(backblaze_b2.Config{
		Endpoint:  endpoint,
		Region:    region,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("backblaze_b2.New: %v", err)
	}
	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"backblaze_b2": p},
		Default:   "backblaze_b2",
	})
}

// TestSuite_CloudflareR2 runs the full S3 compliance suite against a
// Cloudflare R2 bucket.
//
//	R2_ACCOUNT_ID — required unless R2_ENDPOINT is set
//	R2_ENDPOINT   — optional, overrides the derived endpoint
//	R2_BUCKET     — required, throwaway bucket
//	R2_ACCESS_KEY — required
//	R2_SECRET_KEY — required
//
// The test is skipped when R2_BUCKET is unset.
func TestSuite_CloudflareR2(t *testing.T) {
	bucket := os.Getenv("R2_BUCKET")
	if bucket == "" {
		t.Skip("R2_BUCKET not set")
	}
	accessKey := os.Getenv("R2_ACCESS_KEY")
	secretKey := os.Getenv("R2_SECRET_KEY")
	accountID := os.Getenv("R2_ACCOUNT_ID")
	endpoint := os.Getenv("R2_ENDPOINT")
	if accessKey == "" || secretKey == "" || (accountID == "" && endpoint == "") {
		t.Fatalf("R2_BUCKET is set but R2_ACCESS_KEY / R2_SECRET_KEY / R2_ACCOUNT_ID or R2_ENDPOINT are missing")
	}
	p, err := cloudflare_r2.New(cloudflare_r2.Config{
		AccountID: accountID,
		Endpoint:  endpoint,
		Bucket:    bucket,
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("cloudflare_r2.New: %v", err)
	}
	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"cloudflare_r2": p},
		Default:   "cloudflare_r2",
	})
}

// TestSuite_AWSS3 runs the full S3 compliance suite against an AWS S3
// bucket. This entrypoint is the BYOC validation gate: before we
// hand a tenant a role-based BYOC placement policy we validate that
// their bucket accepts our full request matrix.
//
//	AWS_S3_REGION     — required
//	AWS_S3_BUCKET     — required, throwaway bucket
//	AWS_S3_ACCESS_KEY — required
//	AWS_S3_SECRET_KEY — required
//	AWS_S3_ENDPOINT   — optional, override for S3-compatible endpoints
//
// Tests are skipped when AWS_S3_BUCKET is unset.
func TestSuite_AWSS3(t *testing.T) {
	bucket := os.Getenv("AWS_S3_BUCKET")
	if bucket == "" {
		t.Skip("AWS_S3_BUCKET not set")
	}
	region := os.Getenv("AWS_S3_REGION")
	accessKey := os.Getenv("AWS_S3_ACCESS_KEY")
	secretKey := os.Getenv("AWS_S3_SECRET_KEY")
	if region == "" || accessKey == "" || secretKey == "" {
		t.Fatalf("AWS_S3_BUCKET is set but AWS_S3_REGION / AWS_S3_ACCESS_KEY / AWS_S3_SECRET_KEY are missing")
	}
	p, err := aws_s3.New(aws_s3.Config{
		Region:    region,
		Bucket:    bucket,
		Endpoint:  os.Getenv("AWS_S3_ENDPOINT"),
		AccessKey: accessKey,
		SecretKey: secretKey,
	})
	if err != nil {
		t.Fatalf("aws_s3.New: %v", err)
	}
	Run(t, Setup{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"aws_s3": p},
		Default:   "aws_s3",
	})
}
