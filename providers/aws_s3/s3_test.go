package aws_s3

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

// fakeS3 implements s3_generic.S3API just enough to exercise the
// adapter. Each method records the input so tests can assert the
// adapter wired the right Bucket / Key.
type fakeS3 struct {
	putInput    *s3.PutObjectInput
	getInput    *s3.GetObjectInput
	headInput   *s3.HeadObjectInput
	deleteInput *s3.DeleteObjectInput
	listInput   *s3.ListObjectsV2Input
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.putInput = in
	if in.Body != nil {
		_, _ = io.Copy(io.Discard, in.Body)
	}
	return &s3.PutObjectOutput{ETag: aws.String(`"etag"`)}, nil
}
func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.getInput = in
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader([]byte("body")))}, nil
}
func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.headInput = in
	return &s3.HeadObjectOutput{ContentLength: aws.Int64(4), ETag: aws.String(`"etag"`)}, nil
}
func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.deleteInput = in
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.listInput = in
	return &s3.ListObjectsV2Output{}, nil
}

func newTestProvider(t *testing.T, f *fakeS3) *Provider {
	t.Helper()
	p, err := NewWithClient(Config{
		Region:    "ap-southeast-1",
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
		{"missing region", Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}, "Region"},
		{"missing bucket", Config{Region: "r", AccessKey: "a", SecretKey: "s"}, "Bucket"},
		{"missing access key", Config{Region: "r", Bucket: "b", SecretKey: "s"}, "AccessKey"},
		{"missing secret key", Config{Region: "r", Bucket: "b", AccessKey: "a"}, "SecretKey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New(%v) err = %v, want substring %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestPutGetHeadDeleteList_DelegateToS3Generic(t *testing.T) {
	f := &fakeS3{}
	p := newTestProvider(t, f)

	res, err := p.PutPiece(context.Background(), "piece-1", bytes.NewReader([]byte("hello")), providers.PutOptions{ContentType: "text/plain", ContentLength: 5})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if aws.ToString(f.putInput.Bucket) != "bucket" || aws.ToString(f.putInput.Key) != "piece-1" {
		t.Fatalf("PutPiece wiring = %s/%s, want bucket/piece-1", aws.ToString(f.putInput.Bucket), aws.ToString(f.putInput.Key))
	}
	if res.Backend != "aws_s3" {
		t.Fatalf("PutResult.Backend = %q, want aws_s3", res.Backend)
	}

	if _, err := p.GetPiece(context.Background(), "piece-1", nil); err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	if aws.ToString(f.getInput.Key) != "piece-1" {
		t.Fatalf("GetPiece key = %q, want piece-1", aws.ToString(f.getInput.Key))
	}

	if _, err := p.HeadPiece(context.Background(), "piece-1"); err != nil {
		t.Fatalf("HeadPiece: %v", err)
	}
	if aws.ToString(f.headInput.Key) != "piece-1" {
		t.Fatalf("HeadPiece key = %q, want piece-1", aws.ToString(f.headInput.Key))
	}

	if err := p.DeletePiece(context.Background(), "piece-1"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if aws.ToString(f.deleteInput.Key) != "piece-1" {
		t.Fatalf("DeletePiece key = %q, want piece-1", aws.ToString(f.deleteInput.Key))
	}

	if _, err := p.ListPieces(context.Background(), "prefix/", ""); err != nil {
		t.Fatalf("ListPieces: %v", err)
	}
	if aws.ToString(f.listInput.Prefix) != "prefix/" {
		t.Fatalf("ListPieces prefix = %q, want prefix/", aws.ToString(f.listInput.Prefix))
	}
}

func TestPlacementLabels_MapsRegionToCountry(t *testing.T) {
	cases := map[string]string{
		"ap-southeast-1": "SG",
		"eu-west-1":      "IE",
		"us-east-1":      "US",
		"mars-north-1":   "XX",
	}
	for region, want := range cases {
		p, err := NewWithClient(Config{Region: region, Bucket: "b", AccessKey: "a", SecretKey: "s"}, &fakeS3{})
		if err != nil {
			t.Fatalf("NewWithClient(%s): %v", region, err)
		}
		got := p.PlacementLabels().Country
		if got != want {
			t.Errorf("country(%s) = %q, want %q", region, got, want)
		}
	}
}

func TestString_IncludesRegionAndBucket(t *testing.T) {
	p := newTestProvider(t, &fakeS3{})
	if s := p.String(); !strings.Contains(s, "ap-southeast-1") || !strings.Contains(s, "bucket") {
		t.Fatalf("String() = %q, want region and bucket", s)
	}
}
