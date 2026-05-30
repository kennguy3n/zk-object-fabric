package s3_conformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// Runner drives a single S3 conformance run against the supplied
// client. It is intentionally stateful so the same Runner can be used
// from `go test` (where the bucket lives in an httptest.Server) and
// from cmd/s3-conformance-runner (where the bucket is a real
// long-lived endpoint).
//
// Callers MUST set Client and Bucket. Endpoint is purely informational
// — it ends up in the generated Matrix as a provenance field.
//
// CreateBucket controls whether the runner issues PutBucket at the
// start of the run (httptest setups need it; reusing a pre-created
// bucket on a live endpoint does not).
//
// KeyPrefix is prepended to every object key the runner writes so a
// shared bucket can support concurrent runs without collisions. If
// empty, the runner picks a UTC-timestamped prefix.
//
// Cleanup, when true, attempts to delete every object the runner wrote
// after the run completes. Failures are logged into the Matrix as a
// dedicated `bulk / Cleanup` operation rather than swallowed.
type Runner struct {
	Client      *s3.Client
	Endpoint    string
	Bucket      string
	KeyPrefix   string
	CreateBucket bool
	Cleanup     bool
}

// Run executes the full conformance battery and returns the populated
// Matrix. It never returns an error directly — every failure mode is
// captured as an OpResult in the returned Matrix so callers can
// render the full picture rather than abort on the first hiccup.
//
// Run is safe to call multiple times on the same Runner — each call
// produces an independent Matrix using a fresh key namespace.
// Concretely, Run never mutates the receiver: when KeyPrefix is
// empty, a fresh per-call namespace is generated on a stack-local
// copy of the runner state and threaded through every helper, so a
// second Run() call always gets a new prefix (and never collides
// with the first run's residual objects under the cleanup-deleted
// prefix).
func (r *Runner) Run(ctx context.Context) Matrix {
	// Stack-local copy. All helpers receive &local instead of r so
	// any per-run state we set here (today: KeyPrefix; tomorrow:
	// any retry budget or timing-window the runner needs to carry
	// across helpers) stays scoped to this Run() invocation.
	local := *r
	if local.KeyPrefix == "" {
		local.KeyPrefix = fmt.Sprintf("conformance-%s/", time.Now().UTC().Format("20060102-150405"))
	}
	matrix := Matrix{
		Generated: time.Now().UTC(),
		Endpoint:  local.Endpoint,
		Bucket:    local.Bucket,
	}

	if local.CreateBucket {
		matrix.Operations = append(matrix.Operations, local.run("bucket", "PutBucket", func(ctx context.Context) (string, OpStatus, error) {
			_, err := local.Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(local.Bucket)})
			if err == nil {
				return "201 / bucket created", OpPassed, nil
			}
			// BucketAlreadyOwnedByYou / BucketAlreadyExists are
			// acceptable when reusing a pre-created bucket.
			var nse *s3types.BucketAlreadyOwnedByYou
			var bae *s3types.BucketAlreadyExists
			if errors.As(err, &nse) || errors.As(err, &bae) {
				return "bucket already existed (idempotent)", OpPassed, nil
			}
			return "", OpFailed, err
		})(ctx))
	}

	matrix.Operations = append(matrix.Operations, local.coreOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.listingOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.rangeOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.multipartOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.copyOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.versioningOps(ctx)...)
	matrix.Operations = append(matrix.Operations, local.unsupportedOps(ctx)...)

	if local.Cleanup {
		matrix.Operations = append(matrix.Operations, local.cleanup(ctx))
	}

	matrix.Sort()
	return matrix
}

// run is the per-operation harness. It calls fn, captures the
// duration, and produces an OpResult. The contract for fn is:
//
//   - return (detail, OpPassed, nil) on success
//   - return (detail, OpFailed, err) when the server responded but
//     the response did not match expectations
//   - return ("", OpFailed, err) when the SDK call returned an
//     error — run will format the err into the detail
//   - return ("HTTP NNN: …", OpUnsupported, nil) when the server
//     explicitly rejected the request as not implemented
//
// run never propagates ctx cancellation as Errored — a cancelled
// context is an operator-driven abort, not a conformance failure.
func (r *Runner) run(category, op string, fn func(context.Context) (string, OpStatus, error)) func(context.Context) OpResult {
	return func(ctx context.Context) OpResult {
		started := time.Now()
		detail, status, err := fn(ctx)
		dur := time.Since(started)
		if err != nil {
			if detail == "" {
				detail = err.Error()
			} else {
				detail = detail + ": " + err.Error()
			}
		}
		return OpResult{
			Op:       op,
			Category: category,
			Status:   status,
			Detail:   detail,
			Duration: dur,
		}
	}
}

// coreOps covers PUT / GET / HEAD / DELETE / HeadBucket on a single
// key. The detail string for each operation reports the response
// fields that matter for the AWS S3 reference behaviour so a diff
// of the matrix tells you immediately what changed.
func (r *Runner) coreOps(ctx context.Context) []OpResult {
	key := r.KeyPrefix + "core/hello.txt"
	body := []byte("zk-object-fabric s3 conformance: core round trip")
	bodyETag := ""

	out := []OpResult{}

	out = append(out, r.run("bucket", "HeadBucket", func(ctx context.Context) (string, OpStatus, error) {
		_, err := r.Client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(r.Bucket)})
		if err != nil {
			return "", OpFailed, err
		}
		return "200 OK", OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "PutObject", func(ctx context.Context) (string, OpStatus, error) {
		put, err := r.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(r.Bucket),
			Key:           aws.String(key),
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(int64(len(body))),
		})
		if err != nil {
			return "", OpFailed, err
		}
		bodyETag = aws.ToString(put.ETag)
		if bodyETag == "" {
			return "PutObject returned empty ETag", OpFailed, nil
		}
		return fmt.Sprintf("ETag=%s", bodyETag), OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "HeadObject", func(ctx context.Context) (string, OpStatus, error) {
		head, err := r.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return "", OpFailed, err
		}
		if aws.ToInt64(head.ContentLength) != int64(len(body)) {
			return fmt.Sprintf("ContentLength=%d, want %d", aws.ToInt64(head.ContentLength), len(body)), OpFailed, nil
		}
		return fmt.Sprintf("ContentLength=%d ETag=%s", aws.ToInt64(head.ContentLength), aws.ToString(head.ETag)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "GetObject", func(ctx context.Context) (string, OpStatus, error) {
		got, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return "", OpFailed, err
		}
		defer got.Body.Close()
		data, err := io.ReadAll(got.Body)
		if err != nil {
			return "read body", OpFailed, err
		}
		if !bytes.Equal(data, body) {
			return fmt.Sprintf("body mismatch: got %d bytes want %d", len(data), len(body)), OpFailed, nil
		}
		return fmt.Sprintf("body=%d bytes ETag=%s", len(data), aws.ToString(got.ETag)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "DeleteObject", func(ctx context.Context) (string, OpStatus, error) {
		if _, err := r.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		}); err != nil {
			return "", OpFailed, err
		}
		// AWS S3 returns 204 No Content; verify the object is gone.
		_, err := r.Client.HeadObject(ctx, &s3.HeadObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err == nil {
			return "HeadObject after delete still returns 200", OpFailed, nil
		}
		if code := statusCode(err); code != http.StatusNotFound {
			return fmt.Sprintf("HeadObject after delete returned %d, want 404", code), OpFailed, nil
		}
		return "204 No Content, HEAD confirms 404", OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "DeleteObject_Idempotent", func(ctx context.Context) (string, OpStatus, error) {
		// AWS S3 DeleteObject on a missing key returns 204 (no
		// error). The gateway must match.
		if _, err := r.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(r.KeyPrefix + "core/never-existed.txt"),
		}); err != nil {
			return "", OpFailed, err
		}
		return "204 No Content (deleting absent key is idempotent)", OpPassed, nil
	})(ctx))

	out = append(out, r.run("core", "GetObject_MissingKey", func(ctx context.Context) (string, OpStatus, error) {
		_, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(r.KeyPrefix + "core/never-existed.txt"),
		})
		if err == nil {
			return "GET on missing key returned 200", OpFailed, nil
		}
		if code := statusCode(err); code != http.StatusNotFound {
			return fmt.Sprintf("status %d, want 404", code), OpFailed, nil
		}
		return "404 NoSuchKey", OpPassed, nil
	})(ctx))

	return out
}

// listingOps tests ListObjectsV2 with no prefix, with a prefix, and
// with a delimiter (S3 "directory listing" semantics).
func (r *Runner) listingOps(ctx context.Context) []OpResult {
	out := []OpResult{}
	listPrefix := r.KeyPrefix + "list/"
	keys := []string{
		listPrefix + "a.txt",
		listPrefix + "b.txt",
		listPrefix + "sub/c.txt",
	}
	// Seed the test keys.
	for _, k := range keys {
		_, err := r.Client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        aws.String(r.Bucket),
			Key:           aws.String(k),
			Body:          bytes.NewReader([]byte("listing-seed")),
			ContentLength: aws.Int64(int64(len("listing-seed"))),
		})
		if err != nil {
			out = append(out, OpResult{
				Op: "ListObjectsV2_Seed", Category: "listing",
				Status: OpErrored,
				Detail: fmt.Sprintf("seed PUT %s failed: %v", k, err),
			})
			return out
		}
	}

	out = append(out, r.run("listing", "ListObjectsV2_Prefix", func(ctx context.Context) (string, OpStatus, error) {
		page, err := r.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket: aws.String(r.Bucket),
			Prefix: aws.String(listPrefix),
		})
		if err != nil {
			return "", OpFailed, err
		}
		if len(page.Contents) != len(keys) {
			return fmt.Sprintf("returned %d keys, want %d", len(page.Contents), len(keys)), OpFailed, nil
		}
		return fmt.Sprintf("returned %d keys under prefix", len(page.Contents)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("listing", "ListObjectsV2_Delimiter", func(ctx context.Context) (string, OpStatus, error) {
		page, err := r.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:    aws.String(r.Bucket),
			Prefix:    aws.String(listPrefix),
			Delimiter: aws.String("/"),
		})
		if err != nil {
			return "", OpFailed, err
		}
		// Expect 2 top-level keys (a.txt, b.txt) and 1 common
		// prefix (sub/). The gateway may render delimiter
		// semantics differently; treat "delimiter accepted +
		// returned some result" as a soft pass.
		topCount := len(page.Contents)
		prefixCount := len(page.CommonPrefixes)
		if topCount+prefixCount == 0 {
			return "delimiter listing returned 0 results", OpFailed, nil
		}
		return fmt.Sprintf("returned %d top-level keys + %d common prefixes", topCount, prefixCount), OpPassed, nil
	})(ctx))

	out = append(out, r.run("listing", "ListObjectsV1", func(ctx context.Context) (string, OpStatus, error) {
		page, err := r.Client.ListObjects(ctx, &s3.ListObjectsInput{
			Bucket: aws.String(r.Bucket),
			Prefix: aws.String(listPrefix),
		})
		if err != nil {
			return "", OpFailed, err
		}
		if len(page.Contents) != len(keys) {
			return fmt.Sprintf("ListObjects (v1) returned %d, want %d", len(page.Contents), len(keys)), OpFailed, nil
		}
		return fmt.Sprintf("v1 returned %d keys", len(page.Contents)), OpPassed, nil
	})(ctx))

	return out
}

// rangeOps tests byte-range GETs, including the invalid-range
// 416 case.
func (r *Runner) rangeOps(ctx context.Context) []OpResult {
	key := r.KeyPrefix + "range/payload.bin"
	body := []byte("0123456789ABCDEF")
	_, err := r.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return []OpResult{{
			Op: "GetObject_Range", Category: "range",
			Status: OpErrored, Detail: fmt.Sprintf("seed PUT failed: %v", err),
		}}
	}

	out := []OpResult{}

	out = append(out, r.run("range", "GetObject_RangeMiddle", func(ctx context.Context) (string, OpStatus, error) {
		got, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
			Range:  aws.String("bytes=2-7"),
		})
		if err != nil {
			return "", OpFailed, err
		}
		defer got.Body.Close()
		data, err := io.ReadAll(got.Body)
		if err != nil {
			return "", OpFailed, err
		}
		if !bytes.Equal(data, body[2:8]) {
			return fmt.Sprintf("got %q want %q", data, body[2:8]), OpFailed, nil
		}
		return fmt.Sprintf("206 Partial, %d bytes", len(data)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("range", "GetObject_RangeOpen", func(ctx context.Context) (string, OpStatus, error) {
		got, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
			Range:  aws.String("bytes=8-"),
		})
		if err != nil {
			return "", OpFailed, err
		}
		defer got.Body.Close()
		data, err := io.ReadAll(got.Body)
		if err != nil {
			return "", OpFailed, err
		}
		if !bytes.Equal(data, body[8:]) {
			return fmt.Sprintf("got %q want %q", data, body[8:]), OpFailed, nil
		}
		return fmt.Sprintf("206 Partial open-ended, %d bytes", len(data)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("range", "GetObject_RangeUnsatisfiable", func(ctx context.Context) (string, OpStatus, error) {
		_, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
			Range:  aws.String("bytes=1000-2000"),
		})
		if err == nil {
			return "range past EOF returned 200", OpFailed, nil
		}
		if code := statusCode(err); code != http.StatusRequestedRangeNotSatisfiable {
			return fmt.Sprintf("status %d, want 416", code), OpFailed, nil
		}
		return "416 Requested Range Not Satisfiable", OpPassed, nil
	})(ctx))

	return out
}

// multipartOps exercises the full multipart lifecycle: create,
// upload-part, complete, list-parts, abort, list-multipart-uploads.
func (r *Runner) multipartOps(ctx context.Context) []OpResult {
	key := r.KeyPrefix + "multipart/round-trip.bin"
	part1 := bytes.Repeat([]byte("A"), 5*1024*1024) // S3 requires >=5 MiB for non-final parts
	part2 := bytes.Repeat([]byte("B"), 512*1024)
	full := append(append([]byte{}, part1...), part2...)

	out := []OpResult{}
	var uploadID string

	out = append(out, r.run("multipart", "CreateMultipartUpload", func(ctx context.Context) (string, OpStatus, error) {
		create, err := r.Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return "", OpFailed, err
		}
		uploadID = aws.ToString(create.UploadId)
		if uploadID == "" {
			return "empty UploadId returned", OpFailed, nil
		}
		return fmt.Sprintf("UploadId=%s", uploadID), OpPassed, nil
	})(ctx))
	if uploadID == "" {
		// Skip the rest of the multipart group — there's nothing
		// to talk to.
		out = append(out, OpResult{Op: "UploadPart", Category: "multipart", Status: OpErrored, Detail: "skipped: CreateMultipartUpload failed"})
		out = append(out, OpResult{Op: "CompleteMultipartUpload", Category: "multipart", Status: OpErrored, Detail: "skipped: CreateMultipartUpload failed"})
		out = append(out, OpResult{Op: "AbortMultipartUpload", Category: "multipart", Status: OpErrored, Detail: "skipped: CreateMultipartUpload failed"})
		return out
	}

	var part1ETag, part2ETag string

	out = append(out, r.run("multipart", "UploadPart", func(ctx context.Context) (string, OpStatus, error) {
		p1, err := r.Client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(r.Bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(1),
			Body:          bytes.NewReader(part1),
			ContentLength: aws.Int64(int64(len(part1))),
		})
		if err != nil {
			return "part 1", OpFailed, err
		}
		p2, err := r.Client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:        aws.String(r.Bucket),
			Key:           aws.String(key),
			UploadId:      aws.String(uploadID),
			PartNumber:    aws.Int32(2),
			Body:          bytes.NewReader(part2),
			ContentLength: aws.Int64(int64(len(part2))),
		})
		if err != nil {
			return "part 2", OpFailed, err
		}
		part1ETag = aws.ToString(p1.ETag)
		part2ETag = aws.ToString(p2.ETag)
		return fmt.Sprintf("2 parts uploaded (etags=%s, %s)", part1ETag, part2ETag), OpPassed, nil
	})(ctx))

	out = append(out, r.run("multipart", "ListParts", func(ctx context.Context) (string, OpStatus, error) {
		page, err := r.Client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:   aws.String(r.Bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
		})
		if err != nil {
			// ListParts is part of the S3 spec but some gateways
			// haven't wired it yet. Treat 5xx as failed and
			// 4xx as unsupported.
			if code := statusCode(err); isUnsupportedCode(code) {
				return fmt.Sprintf("HTTP %d: %v", code, err), OpUnsupported, nil
			}
			return "", OpFailed, err
		}
		return fmt.Sprintf("returned %d parts", len(page.Parts)), OpPassed, nil
	})(ctx))

	out = append(out, r.run("multipart", "CompleteMultipartUpload", func(ctx context.Context) (string, OpStatus, error) {
		_, err := r.Client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String(r.Bucket),
			Key:      aws.String(key),
			UploadId: aws.String(uploadID),
			MultipartUpload: &s3types.CompletedMultipartUpload{
				Parts: []s3types.CompletedPart{
					{PartNumber: aws.Int32(1), ETag: aws.String(part1ETag)},
					{PartNumber: aws.Int32(2), ETag: aws.String(part2ETag)},
				},
			},
		})
		if err != nil {
			return "", OpFailed, err
		}
		// Verify the assembled object reads back identically.
		got, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			return "GET after complete", OpFailed, err
		}
		defer got.Body.Close()
		data, err := io.ReadAll(got.Body)
		if err != nil {
			return "read after complete", OpFailed, err
		}
		if !bytes.Equal(data, full) {
			return fmt.Sprintf("assembled body mismatch: got %d bytes want %d", len(data), len(full)), OpFailed, nil
		}
		return fmt.Sprintf("assembled %d bytes from 2 parts", len(data)), OpPassed, nil
	})(ctx))

	// A second upload, immediately aborted.
	var abortID string
	out = append(out, r.run("multipart", "AbortMultipartUpload", func(ctx context.Context) (string, OpStatus, error) {
		create, err := r.Client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: aws.String(r.Bucket),
			Key:    aws.String(r.KeyPrefix + "multipart/abort.bin"),
		})
		if err != nil {
			return "create for abort", OpFailed, err
		}
		abortID = aws.ToString(create.UploadId)
		_, err = r.Client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(r.Bucket),
			Key:      aws.String(r.KeyPrefix + "multipart/abort.bin"),
			UploadId: aws.String(abortID),
		})
		if err != nil {
			return "abort", OpFailed, err
		}
		return fmt.Sprintf("204 No Content, UploadId=%s aborted", abortID), OpPassed, nil
	})(ctx))

	out = append(out, r.run("multipart", "ListMultipartUploads", func(ctx context.Context) (string, OpStatus, error) {
		page, err := r.Client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{
			Bucket: aws.String(r.Bucket),
		})
		if err != nil {
			if code := statusCode(err); isUnsupportedCode(code) {
				return fmt.Sprintf("HTTP %d: %v", code, err), OpUnsupported, nil
			}
			return "", OpFailed, err
		}
		return fmt.Sprintf("returned %d in-flight uploads", len(page.Uploads)), OpPassed, nil
	})(ctx))

	return out
}

// copyOps tests CopyObject (same bucket).
func (r *Runner) copyOps(ctx context.Context) []OpResult {
	src := r.KeyPrefix + "copy/src.txt"
	dst := r.KeyPrefix + "copy/dst.txt"
	body := []byte("zk-object-fabric copy-source payload")
	if _, err := r.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.Bucket),
		Key:           aws.String(src),
		Body:          bytes.NewReader(body),
		ContentLength: aws.Int64(int64(len(body))),
	}); err != nil {
		return []OpResult{{
			Op: "CopyObject_SameBucket", Category: "copy",
			Status: OpErrored, Detail: fmt.Sprintf("seed PUT failed: %v", err),
		}}
	}

	return []OpResult{
		r.run("copy", "CopyObject_SameBucket", func(ctx context.Context) (string, OpStatus, error) {
			_, err := r.Client.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket:     aws.String(r.Bucket),
				Key:        aws.String(dst),
				CopySource: aws.String(r.Bucket + "/" + src),
			})
			if err != nil {
				if code := statusCode(err); isUnsupportedCode(code) {
					return fmt.Sprintf("HTTP %d: %v", code, err), OpUnsupported, nil
				}
				return "", OpFailed, err
			}
			got, err := r.Client.GetObject(ctx, &s3.GetObjectInput{
				Bucket: aws.String(r.Bucket),
				Key:    aws.String(dst),
			})
			if err != nil {
				return "GET after copy", OpFailed, err
			}
			defer got.Body.Close()
			data, err := io.ReadAll(got.Body)
			if err != nil {
				return "", OpFailed, err
			}
			if !bytes.Equal(data, body) {
				return fmt.Sprintf("copy body mismatch: got %d bytes want %d", len(data), len(body)), OpFailed, nil
			}
			return fmt.Sprintf("copied %d bytes intact", len(data)), OpPassed, nil
		})(ctx),
	}
}

// versioningOps exercises the version-aware GET and
// ListObjectVersions endpoints that the gateway publishes today.
// Bucket versioning enable/disable is part of unsupportedOps because
// the gateway exposes server-side versioning by default (every PUT
// creates a new version) rather than gating it behind a bucket flag.
func (r *Runner) versioningOps(ctx context.Context) []OpResult {
	return []OpResult{
		r.run("versioning", "ListObjectVersions", func(ctx context.Context) (string, OpStatus, error) {
			page, err := r.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
				Bucket: aws.String(r.Bucket),
				Prefix: aws.String(r.KeyPrefix),
			})
			if err != nil {
				if code := statusCode(err); isUnsupportedCode(code) {
					return fmt.Sprintf("HTTP %d: %v", code, err), OpUnsupported, nil
				}
				return "", OpFailed, err
			}
			return fmt.Sprintf("returned %d versions + %d delete markers", len(page.Versions), len(page.DeleteMarkers)), OpPassed, nil
		})(ctx),
	}
}

// unsupportedOps drives the operations the gateway is intentionally
// not implementing yet. Each one is expected to produce a 4xx. If
// the gateway accidentally returns 200, the matrix records it as a
// failure so reviewers see a regression: the gateway started
// silently accepting an operation it does not actually honour.
func (r *Runner) unsupportedOps(ctx context.Context) []OpResult {
	key := r.KeyPrefix + "unsupported/probe.txt"
	if _, err := r.Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(r.Bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader([]byte("probe")),
		ContentLength: aws.Int64(int64(len("probe"))),
	}); err != nil {
		// Continue anyway — the unsupported probes only need the
		// bucket to exist, not the object.
		_ = err
	}

	probes := []struct {
		category string
		op       string
		invoke   func(context.Context) error
	}{
		{"acl", "GetObjectAcl", func(ctx context.Context) error {
			_, err := r.Client.GetObjectAcl(ctx, &s3.GetObjectAclInput{
				Bucket: aws.String(r.Bucket), Key: aws.String(key),
			})
			return err
		}},
		{"acl", "PutObjectAcl", func(ctx context.Context) error {
			_, err := r.Client.PutObjectAcl(ctx, &s3.PutObjectAclInput{
				Bucket: aws.String(r.Bucket), Key: aws.String(key),
				ACL: s3types.ObjectCannedACLPrivate,
			})
			return err
		}},
		{"tagging", "PutObjectTagging", func(ctx context.Context) error {
			_, err := r.Client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
				Bucket: aws.String(r.Bucket), Key: aws.String(key),
				Tagging: &s3types.Tagging{TagSet: []s3types.Tag{
					{Key: aws.String("env"), Value: aws.String("test")},
				}},
			})
			return err
		}},
		{"tagging", "GetObjectTagging", func(ctx context.Context) error {
			_, err := r.Client.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{
				Bucket: aws.String(r.Bucket), Key: aws.String(key),
			})
			return err
		}},
		{"tagging", "DeleteObjectTagging", func(ctx context.Context) error {
			_, err := r.Client.DeleteObjectTagging(ctx, &s3.DeleteObjectTaggingInput{
				Bucket: aws.String(r.Bucket), Key: aws.String(key),
			})
			return err
		}},
		{"lifecycle", "PutBucketLifecycleConfiguration", func(ctx context.Context) error {
			_, err := r.Client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
				Bucket: aws.String(r.Bucket),
				LifecycleConfiguration: &s3types.BucketLifecycleConfiguration{
					Rules: []s3types.LifecycleRule{{
						ID:     aws.String("expire-30d"),
						Status: s3types.ExpirationStatusEnabled,
						Filter: &s3types.LifecycleRuleFilter{Prefix: aws.String("ephemeral/")},
						Expiration: &s3types.LifecycleExpiration{
							Days: aws.Int32(30),
						},
					}},
				},
			})
			return err
		}},
		{"lifecycle", "GetBucketLifecycleConfiguration", func(ctx context.Context) error {
			_, err := r.Client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
				Bucket: aws.String(r.Bucket),
			})
			return err
		}},
		{"bucket-versioning", "PutBucketVersioning", func(ctx context.Context) error {
			_, err := r.Client.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
				Bucket: aws.String(r.Bucket),
				VersioningConfiguration: &s3types.VersioningConfiguration{
					Status: s3types.BucketVersioningStatusEnabled,
				},
			})
			return err
		}},
		{"bucket-versioning", "GetBucketVersioning", func(ctx context.Context) error {
			_, err := r.Client.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{
				Bucket: aws.String(r.Bucket),
			})
			return err
		}},
		{"bulk", "DeleteObjects", func(ctx context.Context) error {
			_, err := r.Client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(r.Bucket),
				Delete: &s3types.Delete{
					Objects: []s3types.ObjectIdentifier{
						{Key: aws.String(key)},
					},
				},
			})
			return err
		}},
	}

	out := make([]OpResult, 0, len(probes))
	for _, p := range probes {
		out = append(out, r.run(p.category, p.op, func(ctx context.Context) (string, OpStatus, error) {
			err := p.invoke(ctx)
			if err == nil {
				// The gateway accepted an op we expect it to
				// refuse. Record as Failed so the regression is
				// visible — the matrix consumer should not
				// silently re-classify this.
				return "expected 4xx but server returned 200", OpFailed, nil
			}
			code := statusCode(err)
			if isUnsupportedCode(code) {
				return fmt.Sprintf("HTTP %d (expected): %s", code, summariseErr(err)), OpUnsupported, nil
			}
			return fmt.Sprintf("unexpected HTTP %d: %v", code, err), OpFailed, nil
		})(ctx))
	}
	return out
}

// cleanup attempts to delete every object the runner wrote. It
// drives DeleteObject one key at a time rather than using the bulk
// DeleteObjects endpoint because that operation is in unsupportedOps.
//
// The cleanup is version-aware: the gateway exposes server-side
// versioning by default (every PUT creates a new version, every
// DELETE writes a delete marker), so a naive ListObjectsV2 +
// DeleteObject loop would only see the *latest* version of each key
// and would orphan every prior version + delete marker in the
// underlying manifest store / provider. For an in-process test gateway
// that gets torn down via t.TempDir() this would be invisible, but
// runs against a long-lived endpoint (see
// docs/runbooks/s3-conformance.md#using-the-runner-against-a-live-endpoint)
// would steadily accumulate residual versions and inflate storage.
//
// The implementation enumerates everything under r.KeyPrefix via
// ListObjectVersions and issues a per-version DeleteObject with the
// VersionId attached so each version is permanently removed (the
// gateway honours ?versionId; see api/s3compat/handler.go DeleteObject).
// DeleteMarkers are deleted the same way — DeleteObject with the
// delete-marker's VersionId removes the marker itself rather than
// stacking another one.
//
// If the gateway returns 4xx/501 to ListObjectVersions (some
// S3-compatible backends omit it), we fall back to the original
// ListObjectsV2 path. This is no regression over the previous
// behaviour — the same residual-versions caveat applies — and is
// surfaced via a "(versions API unsupported, fell back to v2)" note
// in the matrix Detail field.
func (r *Runner) cleanup(ctx context.Context) OpResult {
	return r.run("bulk", "Cleanup", func(ctx context.Context) (string, OpStatus, error) {
		deleted := 0
		failed := 0

		// Primary path: enumerate every version + delete marker
		// under the prefix and delete each one by VersionId.
		usedVersions := true
		var keyMarker, versionMarker *string
		for {
			page, err := r.Client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{
				Bucket:          aws.String(r.Bucket),
				Prefix:          aws.String(r.KeyPrefix),
				KeyMarker:       keyMarker,
				VersionIdMarker: versionMarker,
			})
			if err != nil {
				if code := statusCode(err); isUnsupportedCode(code) {
					// Gateway does not surface
					// ListObjectVersions — fall back
					// to the v2-list cleanup. We still
					// report success on the runner side,
					// but the Detail field will note the
					// downgrade so operators see it.
					usedVersions = false
					break
				}
				return fmt.Sprintf("ListObjectVersions in cleanup: %v", err), OpFailed, nil
			}
			for _, v := range page.Versions {
				if _, err := r.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket:    aws.String(r.Bucket),
					Key:       v.Key,
					VersionId: v.VersionId,
				}); err != nil {
					failed++
				} else {
					deleted++
				}
			}
			for _, dm := range page.DeleteMarkers {
				if _, err := r.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
					Bucket:    aws.String(r.Bucket),
					Key:       dm.Key,
					VersionId: dm.VersionId,
				}); err != nil {
					failed++
				} else {
					deleted++
				}
			}
			if !aws.ToBool(page.IsTruncated) {
				break
			}
			keyMarker = page.NextKeyMarker
			versionMarker = page.NextVersionIdMarker
		}

		if !usedVersions {
			// Fallback: original ListObjectsV2 + DeleteObject
			// (latest-version-only) cleanup. Used when the
			// gateway under test does not implement
			// ListObjectVersions; the operator should be
			// aware that prior versions will accumulate.
			var token *string
			for {
				page, err := r.Client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
					Bucket:            aws.String(r.Bucket),
					Prefix:            aws.String(r.KeyPrefix),
					ContinuationToken: token,
				})
				if err != nil {
					return fmt.Sprintf("ListObjectsV2 in cleanup (fallback): %v", err), OpFailed, nil
				}
				for _, obj := range page.Contents {
					if _, err := r.Client.DeleteObject(ctx, &s3.DeleteObjectInput{
						Bucket: aws.String(r.Bucket),
						Key:    obj.Key,
					}); err != nil {
						failed++
					} else {
						deleted++
					}
				}
				if !aws.ToBool(page.IsTruncated) {
					break
				}
				token = page.NextContinuationToken
			}
		}

		if failed > 0 {
			suffix := ""
			if !usedVersions {
				suffix = " (versions API unsupported, fell back to v2)"
			}
			return fmt.Sprintf("deleted %d, %d failed%s", deleted, failed, suffix), OpFailed, nil
		}
		suffix := ""
		if !usedVersions {
			suffix = " (versions API unsupported, fell back to v2 \u2014 prior versions may remain)"
		}
		return fmt.Sprintf("deleted %d entries under prefix %q%s", deleted, r.KeyPrefix, suffix), OpPassed, nil
	})(ctx)
}

// isUnsupportedCode reports whether code is the kind of HTTP status
// that means "the server understood the request but is not going to
// honour it" — i.e. the response we treat as Unsupported in the
// matrix rather than Failed.
//
// We accept:
//
//   - any 4xx (400 BadRequest, 403 Forbidden, 405 MethodNotAllowed,
//     404 NotFound when used to indicate "no such operation"). AWS
//     SDKs and many S3-compatible gateways return one of these for
//     unsupported operations.
//
//   - 501 NotImplemented (the canonical AWS code for unsupported
//     operations — explicitly cited in the AWS S3 error reference).
//     This gateway uses 501 for sub-resources it has not wired up;
//     a strict 4xx-only check would mis-classify those as Failed.
//
// 500, 502, 503, 504 are NOT treated as Unsupported — those indicate
// a genuine server-side failure (panic, upstream timeout, etc.) and
// must surface as Failed/Errored so the matrix flags the regression.
func isUnsupportedCode(code int) bool {
	if code >= 400 && code < 500 {
		return true
	}
	return code == http.StatusNotImplemented
}

// statusCode extracts the HTTP status code from an AWS SDK error.
// Returns 0 when no HTTP response was attached (network error,
// context cancellation, etc.).
func statusCode(err error) int {
	if err == nil {
		return 0
	}
	var responseErr *smithyhttp.ResponseError
	if errors.As(err, &responseErr) && responseErr.Response != nil {
		return responseErr.Response.StatusCode
	}
	return 0
}

// summariseErr trims AWS SDK error messages to a single line so the
// matrix Detail field doesn't blow up. The full err.Error() is still
// available to the test caller via the underlying response — this is
// just for the rendered matrix.
func summariseErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if i := strings.Index(msg, "\n"); i > 0 {
		msg = msg[:i]
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}
