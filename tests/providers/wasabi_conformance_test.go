package providers_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
	"github.com/kennguy3n/zk-object-fabric/tests/providers/conformance"
)

// TestStorageProvider_Wasabi runs the shared conformance suite against
// the Wasabi adapter. The backing S3 client is an in-memory fake so
// CI does not need real Wasabi credentials; the same factory pattern
// lets operators swap in a real client by editing one line.
func TestStorageProvider_Wasabi(t *testing.T) {
	factory := func(t *testing.T) providers.StorageProvider {
		t.Helper()
		fake := newFakeS3()
		p, err := wasabi.NewWithClient(wasabi.Config{
			Endpoint:  "https://s3.us-east-1.wasabisys.com",
			Region:    "us-east-1",
			Bucket:    "zk-object-fabric-conformance",
			AccessKey: "test",
			SecretKey: "test",
		}, fake)
		if err != nil {
			t.Fatalf("wasabi.NewWithClient: %v", err)
		}
		return p
	}
	conformance.Run(t, factory, conformance.Options{
		// S3-compatible backends accept slashes and dots in keys, so
		// the filesystem-traversal check is a local_fs_dev concern.
		SkipUnsafePieceIDs: true,
		// S3 DeleteObject is idempotent per spec.
		SkipDeleteMissingError: true,
	})
}

// fakeS3 is an in-memory implementation of s3_generic.S3API. It mimics
// enough of the real Wasabi/S3 surface to exercise the adapter:
// object storage by (bucket, key), range GET, If-None-Match, and
// cursor-paginated List.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[fakeKey]fakeObject
}

type fakeKey struct {
	bucket string
	key    string
}

type fakeObject struct {
	body         []byte
	etag         string
	contentType  string
	storageClass s3types.StorageClass
	metadata     map[string]string
}

func newFakeS3() *fakeS3 {
	return &fakeS3{objects: map[fakeKey]fakeObject{}}
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := fakeKey{bucket: aws.ToString(in.Bucket), key: aws.ToString(in.Key)}
	if aws.ToString(in.IfNoneMatch) == "*" {
		if _, ok := f.objects[key]; ok {
			return nil, errors.New("fakeS3: PreconditionFailed: object already exists")
		}
	}
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, fmt.Errorf("fakeS3: read body: %w", err)
	}
	etag := `"` + strconv.Itoa(len(body)) + `-etag"`
	obj := fakeObject{
		body:         body,
		etag:         etag,
		contentType:  aws.ToString(in.ContentType),
		storageClass: in.StorageClass,
		metadata:     cloneMetadata(in.Metadata),
	}
	f.objects[key] = obj

	size := int64(len(body))
	return &s3.PutObjectOutput{
		ETag: aws.String(etag),
		Size: aws.Int64(size),
	}, nil
}

func (f *fakeS3) GetObject(_ context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.mu.Lock()
	obj, ok := f.objects[fakeKey{bucket: aws.ToString(in.Bucket), key: aws.ToString(in.Key)}]
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("fakeS3: no such key")}
	}

	body := obj.body
	if r := aws.ToString(in.Range); r != "" {
		start, end, err := parseRange(r, int64(len(body)))
		if err != nil {
			return nil, fmt.Errorf("fakeS3: parse range %q: %w", r, err)
		}
		body = body[start : end+1]
	}
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: aws.Int64(int64(len(body))),
		ETag:          aws.String(obj.etag),
		ContentType:   aws.String(obj.contentType),
		StorageClass:  obj.storageClass,
		Metadata:      cloneMetadata(obj.metadata),
	}, nil
}

func (f *fakeS3) HeadObject(_ context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	f.mu.Lock()
	obj, ok := f.objects[fakeKey{bucket: aws.ToString(in.Bucket), key: aws.ToString(in.Key)}]
	f.mu.Unlock()
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("fakeS3: no such key")}
	}
	return &s3.HeadObjectOutput{
		ContentLength: aws.Int64(int64(len(obj.body))),
		ETag:          aws.String(obj.etag),
		ContentType:   aws.String(obj.contentType),
		StorageClass:  obj.storageClass,
		Metadata:      cloneMetadata(obj.metadata),
	}, nil
}

func (f *fakeS3) DeleteObject(_ context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, fakeKey{bucket: aws.ToString(in.Bucket), key: aws.ToString(in.Key)})
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3) ListObjectsV2(_ context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	bucket := aws.ToString(in.Bucket)
	prefix := aws.ToString(in.Prefix)
	cursor := aws.ToString(in.ContinuationToken)

	var keys []string
	for k := range f.objects {
		if k.bucket != bucket {
			continue
		}
		if prefix != "" && !strings.HasPrefix(k.key, prefix) {
			continue
		}
		if cursor != "" && k.key <= cursor {
			continue
		}
		keys = append(keys, k.key)
	}
	sort.Strings(keys)

	contents := make([]s3types.Object, 0, len(keys))
	for _, k := range keys {
		obj := f.objects[fakeKey{bucket: bucket, key: k}]
		contents = append(contents, s3types.Object{
			Key:          aws.String(k),
			Size:         aws.Int64(int64(len(obj.body))),
			ETag:         aws.String(obj.etag),
			StorageClass: s3types.ObjectStorageClass(obj.storageClass),
		})
	}
	return &s3.ListObjectsV2Output{
		Contents:    contents,
		IsTruncated: aws.Bool(false),
	}, nil
}

func (f *fakeS3) CopyObject(_ context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := aws.ToString(in.CopySource)
	// CopySource is "bucket/key" (possibly URL-encoded). Split.
	idx := strings.IndexByte(src, '/')
	if idx <= 0 || idx == len(src)-1 {
		return nil, fmt.Errorf("fakeS3: invalid CopySource %q", src)
	}
	srcBucket, srcKey := src[:idx], src[idx+1:]
	srcObj, ok := f.objects[fakeKey{bucket: srcBucket, key: srcKey}]
	if !ok {
		return nil, &s3types.NoSuchKey{Message: aws.String("fakeS3: no such source key")}
	}
	dstBucket := aws.ToString(in.Bucket)
	dstKey := aws.ToString(in.Key)
	f.objects[fakeKey{bucket: dstBucket, key: dstKey}] = srcObj
	return &s3.CopyObjectOutput{
		CopyObjectResult: &s3types.CopyObjectResult{ETag: aws.String(srcObj.etag)},
	}, nil
}

// parseRange parses an HTTP Range header of the form "bytes=start-end"
// or "bytes=start-". Suffix ranges ("bytes=-N") are not used by the
// fabric so we do not implement them.
func parseRange(h string, size int64) (int64, int64, error) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, fmt.Errorf("invalid range: %q", h)
	}
	spec := strings.TrimPrefix(h, "bytes=")
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, fmt.Errorf("invalid range: %q", h)
	}
	start, err := strconv.ParseInt(spec[:dash], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid range start: %w", err)
	}
	end := size - 1
	if dash < len(spec)-1 {
		e, err := strconv.ParseInt(spec[dash+1:], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("invalid range end: %w", err)
		}
		end = e
	}
	if end >= size {
		end = size - 1
	}
	if start < 0 || start > end {
		return 0, 0, fmt.Errorf("invalid range [%d,%d] for size %d", start, end, size)
	}
	return start, end, nil
}

func cloneMetadata(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
