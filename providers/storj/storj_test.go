package storj

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeUplink is an in-memory UplinkProject used to exercise the
// Storj adapter without the network. Keys are scoped per-bucket so
// a single instance can be shared across subtests safely.
type fakeUplink struct {
	objects map[string]map[string]storedObject // bucket -> key -> object
}

type storedObject struct {
	data     []byte
	etag     string
	ct       string
	class    string
	metadata map[string]string
	created  time.Time
}

func newFakeUplink() *fakeUplink {
	return &fakeUplink{objects: map[string]map[string]storedObject{}}
}

func (f *fakeUplink) UploadObject(
	_ context.Context,
	bucket, key string,
	r io.Reader,
	opts UploadOptions,
) (UploadedObject, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return UploadedObject{}, err
	}
	if f.objects[bucket] == nil {
		f.objects[bucket] = map[string]storedObject{}
	}
	created := time.Now()
	f.objects[bucket][key] = storedObject{
		data:     data,
		etag:     "etag-" + key,
		ct:       opts.ContentType,
		class:    opts.StorageClass,
		metadata: opts.Metadata,
		created:  created,
	}
	return UploadedObject{
		ETag:      "etag-" + key,
		SizeBytes: int64(len(data)),
		CreatedAt: created,
	}, nil
}

func (f *fakeUplink) DownloadObject(
	_ context.Context,
	bucket, key string,
	rng *providers.ByteRange,
) (io.ReadCloser, error) {
	obj, ok := f.objects[bucket][key]
	if !ok {
		return nil, errors.New("not found")
	}
	if rng == nil {
		return io.NopCloser(bytes.NewReader(obj.data)), nil
	}
	end := rng.End
	if end < 0 || end >= int64(len(obj.data)) {
		end = int64(len(obj.data)) - 1
	}
	if rng.Start > end {
		return io.NopCloser(bytes.NewReader(nil)), nil
	}
	return io.NopCloser(bytes.NewReader(obj.data[rng.Start : end+1])), nil
}

func (f *fakeUplink) StatObject(_ context.Context, bucket, key string) (StatResult, error) {
	obj, ok := f.objects[bucket][key]
	if !ok {
		return StatResult{}, errors.New("not found")
	}
	return StatResult{
		ETag:         obj.etag,
		SizeBytes:    int64(len(obj.data)),
		ContentType:  obj.ct,
		StorageClass: obj.class,
		Metadata:     obj.metadata,
	}, nil
}

func (f *fakeUplink) DeleteObject(_ context.Context, bucket, key string) error {
	if _, ok := f.objects[bucket][key]; !ok {
		return errors.New("not found")
	}
	delete(f.objects[bucket], key)
	return nil
}

func (f *fakeUplink) ListObjects(_ context.Context, bucket, prefix, _ string) (ListPage, error) {
	page := ListPage{}
	for k, v := range f.objects[bucket] {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		page.Keys = append(page.Keys, k)
		page.Objects = append(page.Objects, StatResult{
			ETag:         v.etag,
			SizeBytes:    int64(len(v.data)),
			ContentType:  v.ct,
			StorageClass: v.class,
			Metadata:     v.metadata,
		})
	}
	return page, nil
}

func (f *fakeUplink) Close() error { return nil }

func newTestProvider(t *testing.T) (*Provider, *fakeUplink) {
	t.Helper()
	fake := newFakeUplink()
	p, err := NewWithUplink(Config{AccessGrant: "grant", Bucket: "zkf-storj"}, fake)
	if err != nil {
		t.Fatalf("NewWithUplink: %v", err)
	}
	return p, fake
}

func TestValidateConfig(t *testing.T) {
	fake := newFakeUplink()
	// NewWithUplink is the only constructor the package exposes.
	// It runs Config.validate() before touching the uplink project
	// so every validation failure below surfaces from that call.
	if _, err := NewWithUplink(Config{}, fake); err == nil {
		t.Fatal("expected error for empty config")
	}
	if _, err := NewWithUplink(Config{AccessGrant: "g"}, fake); err == nil {
		t.Fatal("expected error for missing bucket")
	}
	if _, err := NewWithUplink(Config{AccessGrant: "g", Bucket: "b"}, nil); err == nil {
		t.Fatal("expected error for nil project")
	}
	if _, err := NewWithUplink(Config{AccessGrant: "g", Bucket: "b"}, fake); err != nil {
		t.Fatalf("expected success for valid config with fake uplink: %v", err)
	}
}

func TestPutAndGetPiece(t *testing.T) {
	ctx := context.Background()
	p, _ := newTestProvider(t)

	payload := []byte("hello zk")
	res, err := p.PutPiece(ctx, "piece-1", bytes.NewReader(payload), providers.PutOptions{
		ContentLength: int64(len(payload)),
		ContentType:   "application/octet-stream",
	})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	if res.Backend != "storj" {
		t.Fatalf("Backend = %q, want storj", res.Backend)
	}
	if res.SizeBytes != int64(len(payload)) {
		t.Fatalf("SizeBytes = %d, want %d", res.SizeBytes, len(payload))
	}

	rc, err := p.GetPiece(ctx, "piece-1", nil)
	if err != nil {
		t.Fatalf("GetPiece: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("round-trip mismatch: %q vs %q", got, payload)
	}
}

func TestGetPieceRange(t *testing.T) {
	ctx := context.Background()
	p, _ := newTestProvider(t)
	_, err := p.PutPiece(ctx, "r", bytes.NewReader([]byte("abcdefgh")), providers.PutOptions{})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	rc, err := p.GetPiece(ctx, "r", &providers.ByteRange{Start: 2, End: 5})
	if err != nil {
		t.Fatalf("GetPiece range: %v", err)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "cdef" {
		t.Fatalf("range read = %q, want cdef", got)
	}
}

func TestHeadAndDelete(t *testing.T) {
	ctx := context.Background()
	p, _ := newTestProvider(t)
	_, err := p.PutPiece(ctx, "head-1", bytes.NewReader([]byte("payload")), providers.PutOptions{})
	if err != nil {
		t.Fatalf("PutPiece: %v", err)
	}
	meta, err := p.HeadPiece(ctx, "head-1")
	if err != nil {
		t.Fatalf("HeadPiece: %v", err)
	}
	if meta.SizeBytes != 7 {
		t.Fatalf("SizeBytes = %d, want 7", meta.SizeBytes)
	}
	if err := p.DeletePiece(ctx, "head-1"); err != nil {
		t.Fatalf("DeletePiece: %v", err)
	}
	if _, err := p.HeadPiece(ctx, "head-1"); err == nil {
		t.Fatal("HeadPiece after DeletePiece should fail")
	}
}

func TestListPieces(t *testing.T) {
	ctx := context.Background()
	p, _ := newTestProvider(t)
	for _, k := range []string{"pfx/a", "pfx/b", "other/c"} {
		_, err := p.PutPiece(ctx, k, bytes.NewReader([]byte("x")), providers.PutOptions{})
		if err != nil {
			t.Fatalf("PutPiece %s: %v", k, err)
		}
	}
	page, err := p.ListPieces(ctx, "pfx/", "")
	if err != nil {
		t.Fatalf("ListPieces: %v", err)
	}
	if len(page.Pieces) != 2 {
		t.Fatalf("Pieces len = %d, want 2 (got %+v)", len(page.Pieces), page.Pieces)
	}
}

func TestCapabilitiesAndCostModel(t *testing.T) {
	p, _ := newTestProvider(t)
	caps := p.Capabilities()
	if !caps.SupportsRangeReads {
		t.Error("Storj must advertise range reads")
	}
	if !caps.SupportsMultipart {
		t.Error("Storj must advertise multipart uploads")
	}
	if caps.SupportsServerSideCopy {
		t.Error("Storj uplink does not offer server-side copy")
	}

	cost := p.CostModel()
	if cost.StorageUSDPerTBMonth != 4.0 {
		t.Errorf("StorageUSDPerTBMonth = %v, want 4.0", cost.StorageUSDPerTBMonth)
	}
	if cost.EgressUSDPerGB != 0.007 {
		t.Errorf("EgressUSDPerGB = %v, want 0.007", cost.EgressUSDPerGB)
	}
}

func TestPlacementLabels(t *testing.T) {
	p, _ := newTestProvider(t)
	labels := p.PlacementLabels()
	if labels.Provider != "storj" {
		t.Errorf("Provider = %q, want storj", labels.Provider)
	}
	if labels.Country != "XX" {
		t.Errorf("Country = %q, want XX", labels.Country)
	}
	if labels.StorageClass != "byoc_decentralized" {
		t.Errorf("StorageClass = %q, want byoc_decentralized", labels.StorageClass)
	}
}
