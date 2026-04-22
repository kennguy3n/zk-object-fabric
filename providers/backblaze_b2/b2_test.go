package backblaze_b2

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

type fakeS3 struct {
	putKey    string
	deleteKey string
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putKey = aws.ToString(in.Key)
	if in.Body != nil {
		_, _ = io.Copy(io.Discard, in.Body)
	}
	return &s3.PutObjectOutput{ETag: aws.String(`"etag"`)}, nil
}
func (f *fakeS3) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
func (f *fakeS3) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{}, nil
}
func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.deleteKey = aws.ToString(in.Key)
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}

func newTestProvider(t *testing.T, f *fakeS3) *Provider {
	t.Helper()
	p, err := NewWithClient(Config{
		Endpoint:  "https://s3.us-west-002.backblazeb2.com",
		Region:    "us-west-002",
		Bucket:    "bucket",
		AccessKey: "ak",
		SecretKey: "sk",
	}, f)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return p
}

func TestNew_ValidatesConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing endpoint", Config{Region: "r", Bucket: "b", AccessKey: "a", SecretKey: "s"}, "Endpoint"},
		{"missing region", Config{Endpoint: "e", Bucket: "b", AccessKey: "a", SecretKey: "s"}, "Region"},
		{"missing bucket", Config{Endpoint: "e", Region: "r", AccessKey: "a", SecretKey: "s"}, "Bucket"},
		{"missing creds", Config{Endpoint: "e", Region: "r", Bucket: "b"}, "AccessKey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New(%v) err = %v, want substring %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestPutDelete_DelegateToS3Generic(t *testing.T) {
	f := &fakeS3{}
	p := newTestProvider(t, f)
	if _, err := p.PutPiece(context.Background(), "piece-1", bytes.NewReader([]byte("x")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if f.putKey != "piece-1" {
		t.Fatalf("putKey = %q, want piece-1", f.putKey)
	}
	if err := p.DeletePiece(context.Background(), "piece-1"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if f.deleteKey != "piece-1" {
		t.Fatalf("deleteKey = %q, want piece-1", f.deleteKey)
	}
}

func TestCapabilities_ZeroMinStorage(t *testing.T) {
	p := newTestProvider(t, &fakeS3{})
	if caps := p.Capabilities(); caps.MinStorageDurationDays != 0 {
		t.Fatalf("MinStorageDurationDays = %d, want 0", caps.MinStorageDurationDays)
	}
}

func TestPlacementLabels_EURegionMapsToNL(t *testing.T) {
	p, err := NewWithClient(Config{
		Endpoint:  "https://s3.eu-central-003.backblazeb2.com",
		Region:    "eu-central-003",
		Bucket:    "b",
		AccessKey: "a",
		SecretKey: "s",
	}, &fakeS3{})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if c := p.PlacementLabels().Country; c != "NL" {
		t.Fatalf("Country = %q, want NL", c)
	}
}
