package cloudflare_r2

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
	putKey string
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
func (f *fakeS3) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}
func (f *fakeS3) CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	return &s3.CopyObjectOutput{}, nil
}

func TestNew_DerivesEndpointFromAccountID(t *testing.T) {
	p, err := NewWithClient(Config{
		AccountID: "abcdef",
		Bucket:    "b",
		AccessKey: "a",
		SecretKey: "s",
	}, &fakeS3{})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if !strings.Contains(p.endpoint, "abcdef.r2.cloudflarestorage.com") {
		t.Fatalf("endpoint = %q, want contains abcdef.r2.cloudflarestorage.com", p.endpoint)
	}
}

func TestNew_ValidatesBucketAndCreds(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing bucket", Config{AccountID: "x", AccessKey: "a", SecretKey: "s"}, "Bucket"},
		{"missing account and endpoint", Config{Bucket: "b", AccessKey: "a", SecretKey: "s"}, "AccountID"},
		{"missing access key", Config{AccountID: "x", Bucket: "b", SecretKey: "s"}, "AccessKey"},
		{"missing secret key", Config{AccountID: "x", Bucket: "b", AccessKey: "a"}, "SecretKey"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := New(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New(%v) err = %v, want substring %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestPutPiece_DelegatesToS3Generic(t *testing.T) {
	f := &fakeS3{}
	p, err := NewWithClient(Config{AccountID: "x", Bucket: "b", AccessKey: "a", SecretKey: "s"}, f)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	if _, err := p.PutPiece(context.Background(), "piece-1", bytes.NewReader([]byte("x")), providers.PutOptions{}); err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if f.putKey != "piece-1" {
		t.Fatalf("putKey = %q, want piece-1", f.putKey)
	}
}

func TestPlacementLabels_RegionAutoCountryXX(t *testing.T) {
	p, err := NewWithClient(Config{AccountID: "x", Bucket: "b", AccessKey: "a", SecretKey: "s"}, &fakeS3{})
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	labels := p.PlacementLabels()
	if labels.Region != "auto" {
		t.Fatalf("Region = %q, want auto", labels.Region)
	}
	if labels.Country != "XX" {
		t.Fatalf("Country = %q, want XX", labels.Country)
	}
}

func TestEffectivePathStyle_DefaultsTrue(t *testing.T) {
	if !(Config{}).effectivePathStyle() {
		t.Fatal("path-style should default to true")
	}
	if (Config{DisablePathStyle: true}).effectivePathStyle() {
		t.Fatal("DisablePathStyle should flip path-style to false")
	}
}
