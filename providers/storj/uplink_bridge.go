// uplink_bridge.go adapts a real storj.io/uplink.Project to the
// narrow UplinkProject interface the adapter depends on. Operators
// wire the bridge in cmd/gateway/main.go; tests continue to use the
// fake defined in storj_test.go so the compliance suite does not
// require live Storj credentials.
//
// The bridge is an intentionally thin line-for-line translation of
// uplink's public API: UploadObject streams r through *uplink.Upload
// and calls Commit; DownloadObject forwards byte ranges through
// uplink.DownloadOptions; ListObjects drains a single page of
// uplink.ObjectIterator into the StatResult shape the adapter
// expects.
//
// Satellite overrides: storj.io/uplink embeds the satellite URL in
// the access grant, and the public API does not expose a way to
// override it after parsing. cfg.SatelliteAddress is therefore
// ignored by the bridge today; production deployments must mint an
// access grant that already points at the desired satellite.

package storj

import (
	"context"
	"errors"
	"fmt"
	"io"

	"storj.io/uplink"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// OpenUplinkProject parses cfg.AccessGrant into an *uplink.Access
// and opens an *uplink.Project, returning an UplinkProject bridge
// ready for NewWithUplink. It is the production constructor used
// by cmd/gateway/main.go.
func OpenUplinkProject(ctx context.Context, cfg Config) (UplinkProject, error) {
	if cfg.AccessGrant == "" {
		return nil, errors.New("storj: access_grant is required")
	}
	access, err := uplink.ParseAccess(cfg.AccessGrant)
	if err != nil {
		return nil, fmt.Errorf("storj: parse access grant: %w", err)
	}
	project, err := uplink.OpenProject(ctx, access)
	if err != nil {
		return nil, fmt.Errorf("storj: open project: %w", err)
	}
	return &uplinkBridge{project: project}, nil
}

// uplinkBridge translates the narrow UplinkProject interface into
// calls against storj.io/uplink.Project.
type uplinkBridge struct {
	project *uplink.Project
}

// UploadObject streams r into (bucket, key) via *uplink.Upload and
// commits the upload. On any Write/Commit error the upload is
// aborted so no dangling segments are left on the satellite.
func (b *uplinkBridge) UploadObject(
	ctx context.Context,
	bucket, key string,
	r io.Reader,
	opts UploadOptions,
) (UploadedObject, error) {
	up, err := b.project.UploadObject(ctx, bucket, key, nil)
	if err != nil {
		return UploadedObject{}, fmt.Errorf("uplink upload start: %w", err)
	}
	if _, err := io.Copy(up, r); err != nil {
		_ = up.Abort()
		return UploadedObject{}, fmt.Errorf("uplink upload copy: %w", err)
	}
	// The S3 data plane emits PutOptions with Metadata=nil but
	// ContentType set (see api/s3compat/handler.go); guarding only
	// on len(opts.Metadata) would silently drop the content type on
	// every normal PUT, so the guard considers both fields.
	if len(opts.Metadata) > 0 || opts.ContentType != "" {
		custom := uplink.CustomMetadata{}
		for k, v := range opts.Metadata {
			custom[k] = v
		}
		if opts.ContentType != "" {
			custom["content-type"] = opts.ContentType
		}
		if err := up.SetCustomMetadata(ctx, custom); err != nil {
			_ = up.Abort()
			return UploadedObject{}, fmt.Errorf("uplink upload metadata: %w", err)
		}
	}
	if err := up.Commit(); err != nil {
		return UploadedObject{}, fmt.Errorf("uplink upload commit: %w", err)
	}
	info := up.Info()
	if info == nil {
		return UploadedObject{}, errors.New("uplink upload commit returned no object info")
	}
	return UploadedObject{
		SizeBytes: info.System.ContentLength,
		CreatedAt: info.System.Created,
	}, nil
}

// DownloadObject returns a reader over (bucket, key), forwarding
// byte ranges via uplink.DownloadOptions.
func (b *uplinkBridge) DownloadObject(
	ctx context.Context,
	bucket, key string,
	rng *providers.ByteRange,
) (io.ReadCloser, error) {
	var opts *uplink.DownloadOptions
	if rng != nil {
		length := int64(-1)
		if rng.End >= 0 {
			length = rng.End - rng.Start + 1
		}
		opts = &uplink.DownloadOptions{Offset: rng.Start, Length: length}
	}
	dl, err := b.project.DownloadObject(ctx, bucket, key, opts)
	if err != nil {
		return nil, fmt.Errorf("uplink download: %w", err)
	}
	return dl, nil
}

// StatObject returns metadata for (bucket, key) without
// transferring bytes.
func (b *uplinkBridge) StatObject(ctx context.Context, bucket, key string) (StatResult, error) {
	obj, err := b.project.StatObject(ctx, bucket, key)
	if err != nil {
		return StatResult{}, fmt.Errorf("uplink stat: %w", err)
	}
	return statResultFromObject(obj), nil
}

// DeleteObject removes (bucket, key). Missing objects are not
// treated as an error because the adapter's DeletePiece contract
// is idempotent.
func (b *uplinkBridge) DeleteObject(ctx context.Context, bucket, key string) error {
	if _, err := b.project.DeleteObject(ctx, bucket, key); err != nil {
		if errors.Is(err, uplink.ErrObjectNotFound) {
			return nil
		}
		return fmt.Errorf("uplink delete: %w", err)
	}
	return nil
}

// ListObjects drains one page of the uplink ObjectIterator starting
// at cursor. Storj's native iterator does not expose a per-page
// cursor directly, so the bridge paginates one object at a time
// and stops after a single iteration step — callers that need
// larger pages can call ListPieces repeatedly.
func (b *uplinkBridge) ListObjects(
	ctx context.Context,
	bucket, prefix, cursor string,
) (ListPage, error) {
	it := b.project.ListObjects(ctx, bucket, &uplink.ListObjectsOptions{
		Prefix:    prefix,
		Cursor:    cursor,
		Recursive: true,
		System:    true,
		Custom:    true,
	})
	var page ListPage
	for it.Next() {
		obj := it.Item()
		if obj == nil || obj.IsPrefix {
			continue
		}
		page.Objects = append(page.Objects, statResultFromObject(obj))
		page.Keys = append(page.Keys, obj.Key)
		page.NextCursor = obj.Key
	}
	if err := it.Err(); err != nil {
		return ListPage{}, fmt.Errorf("uplink list: %w", err)
	}
	return page, nil
}

// Close releases the underlying uplink.Project.
func (b *uplinkBridge) Close() error {
	if b.project == nil {
		return nil
	}
	return b.project.Close()
}

func statResultFromObject(obj *uplink.Object) StatResult {
	if obj == nil {
		return StatResult{}
	}
	meta := map[string]string{}
	for k, v := range obj.Custom {
		meta[k] = v
	}
	contentType := meta["content-type"]
	return StatResult{
		SizeBytes:   obj.System.ContentLength,
		ContentType: contentType,
		Metadata:    meta,
	}
}
