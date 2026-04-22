package s3_generic

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeS3 is a minimal S3API that records inputs and returns canned
// responses with quoted ETags — the shape the real S3 service uses.
type fakeS3 struct {
	putETag  string
	headETag string
	listETag string
}

func (f *fakeS3) PutObject(_ context.Context, _ *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{ETag: aws.String(f.putETag)}, nil
}

func (f *fakeS3) GetObject(_ context.Context, _ *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, _ *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{ETag: aws.String(f.headETag)}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, _ *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{
		Contents: []s3types.Object{{
			Key:  aws.String("piece"),
			ETag: aws.String(f.listETag),
		}},
	}, nil
}

func newTestProvider(t *testing.T, fake *fakeS3) *Provider {
	t.Helper()
	p, err := NewWithClient(Config{
		Name:      "test",
		Region:    "us-east-1",
		Bucket:    "bucket",
		AccessKey: "ak",
		SecretKey: "sk",
	}, fake)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return p
}

func TestNormalizeETag(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`"abc"`, "abc"},
		{"abc", "abc"},
		{`""`, ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := normalizeETag(tc.in); got != tc.want {
			t.Errorf("normalizeETag(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestETagNormalizedConsistently(t *testing.T) {
	const quoted = `"deadbeef"`
	const want = "deadbeef"

	fake := &fakeS3{putETag: quoted, headETag: quoted, listETag: quoted}
	p := newTestProvider(t, fake)
	ctx := context.Background()

	put, err := p.PutPiece(ctx, "piece", bytes.NewReader([]byte("x")), providers.PutOptions{})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if put.ETag != want {
		t.Errorf("PutPiece ETag = %q, want %q", put.ETag, want)
	}

	head, err := p.HeadPiece(ctx, "piece")
	if err != nil {
		t.Fatalf("HeadPiece: %v", err)
	}
	if head.ETag != want {
		t.Errorf("HeadPiece ETag = %q, want %q", head.ETag, want)
	}

	list, err := p.ListPieces(ctx, "", "")
	if err != nil {
		t.Fatalf("ListPieces: %v", err)
	}
	if len(list.Pieces) != 1 || list.Pieces[0].ETag != want {
		t.Errorf("ListPieces ETag = %+v, want %q", list.Pieces, want)
	}
}
