package wasabi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeS3 is the bare minimum S3API we need to exercise the Wasabi
// adapter's DeletePiece override. It records whether DeleteObject
// was invoked so the test can assert the delete still went through.
type fakeS3 struct {
	deleteCalled bool
}

func (f *fakeS3) PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	return &s3.PutObjectOutput{ETag: aws.String(`"etag"`)}, nil
}
func (f *fakeS3) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(nil))}, nil
}
func (f *fakeS3) HeadObject(context.Context, *s3.HeadObjectInput, ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	return &s3.HeadObjectOutput{ETag: aws.String(`"etag"`)}, nil
}
func (f *fakeS3) DeleteObject(context.Context, *s3.DeleteObjectInput, ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.deleteCalled = true
	return &s3.DeleteObjectOutput{}, nil
}
func (f *fakeS3) ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{}, nil
}
func (f *fakeS3) CopyObject(context.Context, *s3.CopyObjectInput, ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	return &s3.CopyObjectOutput{}, nil
}

func newTestProvider(t *testing.T, fake *fakeS3) *Provider {
	t.Helper()
	p, err := NewWithClient(Config{
		Endpoint:  "https://s3.test.wasabisys.com",
		Region:    "ap-southeast-1",
		Bucket:    "bucket",
		AccessKey: "ak",
		SecretKey: "sk",
	}, fake)
	if err != nil {
		t.Fatalf("NewWithClient: %v", err)
	}
	return p
}

func TestDeletePiece_EmitsWarningBeforeMinStorageDuration(t *testing.T) {
	fake := &fakeS3{}
	p := newTestProvider(t, fake)

	var buf bytes.Buffer
	p.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p.AgeLookup = func(_ context.Context, pieceID string) (time.Duration, bool) {
		if pieceID != "piece-1" {
			return 0, false
		}
		return 7 * 24 * time.Hour, true
	}

	if err := p.DeletePiece(context.Background(), "piece-1"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeletePiece did not issue the backend delete")
	}
	log := buf.String()
	if !strings.Contains(log, "90-day minimum storage duration") {
		t.Errorf("missing 90-day warning in log: %q", log)
	}
	if !strings.Contains(log, "piece_id=piece-1") {
		t.Errorf("missing piece_id in log: %q", log)
	}
}

func TestDeletePiece_NoWarningAfterMinStorageDuration(t *testing.T) {
	fake := &fakeS3{}
	p := newTestProvider(t, fake)

	var buf bytes.Buffer
	p.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p.AgeLookup = func(context.Context, string) (time.Duration, bool) {
		return (WasabiMinStorageDays + 1) * 24 * time.Hour, true
	}

	if err := p.DeletePiece(context.Background(), "piece-2"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeletePiece did not issue the backend delete")
	}
	if strings.Contains(buf.String(), "90-day") {
		t.Errorf("unexpected 90-day warning for aged piece: %q", buf.String())
	}
}

func TestDeletePiece_NoAgeLookupDoesNotPanic(t *testing.T) {
	fake := &fakeS3{}
	p := newTestProvider(t, fake)
	// AgeLookup intentionally left nil.
	if err := p.DeletePiece(context.Background(), "piece-3"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if !fake.deleteCalled {
		t.Fatal("DeletePiece did not issue the backend delete")
	}
}
