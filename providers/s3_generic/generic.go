// Package s3_generic is the shared S3-compatible StorageProvider
// base.
//
// Adapters for AWS S3, Wasabi, Backblaze B2, Cloudflare R2, and any
// other S3-compatible service embed *Provider and only override the
// things that differ (capabilities, cost model, placement labels,
// endpoint quirks).
//
// This file implements PutPiece / GetPiece / HeadPiece / DeletePiece
// / ListPieces against github.com/aws/aws-sdk-go-v2/service/s3.
package s3_generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// Config captures the fields every S3-compatible adapter needs.
type Config struct {
	// Name is the provider label used in PutResult.Backend and
	// PlacementLabels.Provider. Adapters that embed Provider should
	// set this to their provider slug (e.g. "wasabi", "aws_s3").
	Name string
	// Endpoint is the service endpoint URL, e.g.
	// "https://s3.ap-southeast-1.wasabisys.com". Empty means use the
	// default AWS endpoint resolver.
	Endpoint string
	// Region is the region label used for Sigv4 signing.
	Region string
	// Bucket is the single bucket this adapter instance operates on.
	Bucket string
	// AccessKey / SecretKey are static service credentials. They are
	// never logged.
	AccessKey string
	SecretKey string
	// UsePathStyle forces path-style addressing (required by some
	// S3-compatible services). Default is virtual-hosted style.
	UsePathStyle bool
}

// S3API is the subset of s3.Client this package uses. Keeping it as
// an interface lets tests inject a fake without spinning up a real
// HTTP mock.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	CopyObject(ctx context.Context, in *s3.CopyObjectInput, opts ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
}

// Provider is the shared S3-compatible StorageProvider.
type Provider struct {
	cfg    Config
	client S3API
}

// New returns a Provider backed by a freshly constructed s3.Client.
func New(cfg Config) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	client := s3.New(s3.Options{
		Region:       cfg.Region,
		BaseEndpoint: endpointPtr(cfg.Endpoint),
		UsePathStyle: cfg.UsePathStyle,
		Credentials:  credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
	})
	return &Provider{cfg: cfg, client: client}, nil
}

// NewWithClient returns a Provider using a caller-supplied S3API. This
// is the seam tests and embedders use to inject a fake.
func NewWithClient(cfg Config, client S3API) (*Provider, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("s3_generic: client is required")
	}
	return &Provider{cfg: cfg, client: client}, nil
}

func (c Config) validate() error {
	if c.Region == "" {
		return errors.New("s3_generic: region is required")
	}
	if c.Bucket == "" {
		return errors.New("s3_generic: bucket is required")
	}
	if c.AccessKey == "" || c.SecretKey == "" {
		return errors.New("s3_generic: access_key and secret_key are required")
	}
	return nil
}

func endpointPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// BucketName returns the configured bucket.
func (p *Provider) BucketName() string { return p.cfg.Bucket }

// ProviderName returns the configured provider label.
func (p *Provider) ProviderName() string { return p.cfg.Name }

// RegionName returns the configured region.
func (p *Provider) RegionName() string { return p.cfg.Region }

// Client exposes the underlying S3API for embedders that need to
// issue direct calls (e.g. multipart upload).
func (p *Provider) Client() S3API { return p.client }

// PutPiece uploads ciphertext to s3://{bucket}/{pieceID}.
func (p *Provider) PutPiece(ctx context.Context, pieceID string, r io.Reader, opts providers.PutOptions) (providers.PutResult, error) {
	if pieceID == "" {
		return providers.PutResult{}, errors.New("s3_generic: pieceID is required")
	}
	in := &s3.PutObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(pieceID),
		Body:   r,
	}
	if opts.ContentType != "" {
		in.ContentType = aws.String(opts.ContentType)
	}
	if opts.ContentLength > 0 {
		in.ContentLength = aws.Int64(opts.ContentLength)
	}
	if opts.StorageClass != "" {
		in.StorageClass = s3types.StorageClass(opts.StorageClass)
	}
	if len(opts.Metadata) != 0 {
		in.Metadata = opts.Metadata
	}
	if opts.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}

	// Swap the default payload-hash middleware for the
	// UNSIGNED-PAYLOAD variant so PutPiece works with any
	// io.Reader. Without this the signer attempts to hash the
	// body up-front and then seek back to the start to replay
	// it on the wire; non-seekable streams (the
	// r.Body from a multipart part request, a LimitReader
	// wrapping the request body, etc.) fail with "failed to
	// compute payload hash: failed to seek body to start".
	//
	// This is the AWS SDK v2 documented escape hatch for
	// non-AWS S3-compatible endpoints that are happy with
	// UNSIGNED-PAYLOAD as long as the rest of the SigV4
	// envelope is valid. Backend integrity is still verified
	// by ETag comparison on the caller side, so dropping the
	// content hash here is not a security regression for the
	// gateway.
	out, err := p.client.PutObject(ctx, in, s3.WithAPIOptions(
		v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware,
	))
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("s3_generic: put %s/%s: %w", p.cfg.Bucket, pieceID, err)
	}

	size := opts.ContentLength
	if out.Size != nil {
		size = *out.Size
	}
	return providers.PutResult{
		PieceID:   pieceID,
		ETag:      normalizeETag(aws.ToString(out.ETag)),
		SizeBytes: size,
		Backend:   p.cfg.Name,
		Locator:   fmt.Sprintf("s3://%s/%s", p.cfg.Bucket, pieceID),
	}, nil
}

// GetPiece fetches the piece body, honouring byteRange if non-nil.
func (p *Provider) GetPiece(ctx context.Context, pieceID string, byteRange *providers.ByteRange) (io.ReadCloser, error) {
	if pieceID == "" {
		return nil, errors.New("s3_generic: pieceID is required")
	}
	in := &s3.GetObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(pieceID),
	}
	if byteRange != nil {
		in.Range = aws.String(formatRange(byteRange))
	}
	out, err := p.client.GetObject(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("s3_generic: get %s/%s: %w", p.cfg.Bucket, pieceID, err)
	}
	return out.Body, nil
}

// HeadPiece projects s3.HeadObject output into PieceMetadata.
func (p *Provider) HeadPiece(ctx context.Context, pieceID string) (providers.PieceMetadata, error) {
	if pieceID == "" {
		return providers.PieceMetadata{}, errors.New("s3_generic: pieceID is required")
	}
	out, err := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(pieceID),
	})
	if err != nil {
		return providers.PieceMetadata{}, fmt.Errorf("s3_generic: head %s/%s: %w", p.cfg.Bucket, pieceID, err)
	}
	return providers.PieceMetadata{
		PieceID:      pieceID,
		SizeBytes:    aws.ToInt64(out.ContentLength),
		ETag:         normalizeETag(aws.ToString(out.ETag)),
		ContentType:  aws.ToString(out.ContentType),
		StorageClass: string(out.StorageClass),
		Metadata:     out.Metadata,
	}, nil
}

// DeletePiece removes a single object.
func (p *Provider) DeletePiece(ctx context.Context, pieceID string) error {
	if pieceID == "" {
		return errors.New("s3_generic: pieceID is required")
	}
	_, err := p.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(pieceID),
	})
	if err != nil {
		return fmt.Errorf("s3_generic: delete %s/%s: %w", p.cfg.Bucket, pieceID, err)
	}
	return nil
}

// CopyPiece performs an S3 server-side CopyObject. Both source
// and destination must be in the same bucket configured on this
// Provider; cross-bucket copies require operating against a
// different Provider instance pointed at the destination bucket.
//
// The returned PutResult mirrors a fresh PUT: PieceID is the new
// object key, ETag is the server-side hash, SizeBytes is queried
// from the destination via HeadObject so callers can persist it
// in the manifest.
func (p *Provider) CopyPiece(ctx context.Context, srcPieceID, dstPieceID string) (providers.PutResult, error) {
	if srcPieceID == "" || dstPieceID == "" {
		return providers.PutResult{}, errors.New("s3_generic: srcPieceID and dstPieceID are required")
	}
	// CopySource must be url-encoded "bucket/key".
	src := p.cfg.Bucket + "/" + srcPieceID
	out, err := p.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(p.cfg.Bucket),
		Key:        aws.String(dstPieceID),
		CopySource: aws.String(src),
	})
	if err != nil {
		return providers.PutResult{}, fmt.Errorf("s3_generic: copy %s -> %s/%s: %w", src, p.cfg.Bucket, dstPieceID, err)
	}
	res := providers.PutResult{
		PieceID: dstPieceID,
		Backend: p.cfg.Name,
		Locator: p.cfg.Bucket + "/" + dstPieceID,
	}
	if out.CopyObjectResult != nil {
		res.ETag = normalizeETag(aws.ToString(out.CopyObjectResult.ETag))
	}
	// CopyObject does not return content length; query it.
	head, herr := p.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(p.cfg.Bucket),
		Key:    aws.String(dstPieceID),
	})
	if herr == nil && head != nil {
		res.SizeBytes = aws.ToInt64(head.ContentLength)
		if res.ETag == "" {
			res.ETag = normalizeETag(aws.ToString(head.ETag))
		}
	}
	return res, nil
}

// ListPieces paginates object IDs under prefix. cursor is the
// continuation token returned by a previous call.
func (p *Provider) ListPieces(ctx context.Context, prefix, cursor string) (providers.ListResult, error) {
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(p.cfg.Bucket),
	}
	if prefix != "" {
		in.Prefix = aws.String(prefix)
	}
	if cursor != "" {
		in.ContinuationToken = aws.String(cursor)
	}
	out, err := p.client.ListObjectsV2(ctx, in)
	if err != nil {
		return providers.ListResult{}, fmt.Errorf("s3_generic: list %s prefix=%q: %w", p.cfg.Bucket, prefix, err)
	}
	pieces := make([]providers.PieceMetadata, 0, len(out.Contents))
	for _, obj := range out.Contents {
		pieces = append(pieces, providers.PieceMetadata{
			PieceID:      aws.ToString(obj.Key),
			SizeBytes:    aws.ToInt64(obj.Size),
			ETag:         normalizeETag(aws.ToString(obj.ETag)),
			StorageClass: string(obj.StorageClass),
		})
	}
	var next string
	if aws.ToBool(out.IsTruncated) {
		next = aws.ToString(out.NextContinuationToken)
	}
	return providers.ListResult{Pieces: pieces, NextCursor: next}, nil
}

// Capabilities reports the S3-compatible subset. Embedders override
// this to narrow or widen the envelope.
func (p *Provider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{
		SupportsRangeReads:     true,
		SupportsMultipart:      true,
		SupportsIfNoneMatch:    true,
		SupportsServerSideCopy: true,
		MaxObjectSizeBytes:     5 * 1024 * 1024 * 1024 * 1024, // 5 TiB
	}
}

// CostModel returns a zero-cost model; concrete adapters override.
func (p *Provider) CostModel() providers.ProviderCostModel {
	return providers.ProviderCostModel{}
}

// PlacementLabels reports provider + region. Concrete adapters
// enrich with country, failure zone, and storage class.
func (p *Provider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{
		Provider: p.cfg.Name,
		Region:   p.cfg.Region,
	}
}

// normalizeETag strips the surrounding double quotes that S3 wraps
// ETag values in. Applied consistently in PutPiece, HeadPiece, and
// ListPieces so callers see the same shape regardless of which call
// surfaced the ETag.
func normalizeETag(etag string) string {
	return strings.Trim(etag, `"`)
}

// formatRange builds an HTTP Range header from a ByteRange.
// End == -1 is rendered as an open-ended "bytes=start-" request.
func formatRange(r *providers.ByteRange) string {
	if r.End < 0 {
		return "bytes=" + strconv.FormatInt(r.Start, 10) + "-"
	}
	return "bytes=" + strconv.FormatInt(r.Start, 10) + "-" + strconv.FormatInt(r.End, 10)
}

var _ providers.StorageProvider = (*Provider)(nil)
