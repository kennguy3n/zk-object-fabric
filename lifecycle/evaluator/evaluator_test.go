package evaluator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// ---- mocks ----

type mockSource struct {
	entries    []bucket_config.LifecycleEntry
	versioning map[string]bucket_config.VersioningState // key: tenant\x00bucket
	listErr    error
}

func (m *mockSource) ListLifecycle(context.Context) ([]bucket_config.LifecycleEntry, error) {
	return m.entries, m.listErr
}

func (m *mockSource) GetVersioning(_ context.Context, tenantID, bucket string) (bucket_config.VersioningState, error) {
	if st, ok := m.versioning[tenantID+"\x00"+bucket]; ok {
		return st, nil
	}
	return bucket_config.VersioningState(""), nil
}

// mockManifests holds latest-version manifests per (tenant,bucket) for
// List, and per-objectKeyHash version lists for ListVersions.
type mockManifests struct {
	mu       sync.Mutex
	latest   map[string][]*metadata.ObjectManifest // key: tenant\x00bucket
	versions map[string][]*metadata.ObjectManifest // key: objectKeyHash
	puts     []*metadata.ObjectManifest
	deletes  []manifest_store.ManifestKey
}

func newMockManifests() *mockManifests {
	return &mockManifests{
		latest:   map[string][]*metadata.ObjectManifest{},
		versions: map[string][]*metadata.ObjectManifest{},
	}
}

func (m *mockManifests) List(_ context.Context, tenantID, bucket, _ string, _ int) (manifest_store.ListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return manifest_store.ListResult{Manifests: m.latest[tenantID+"\x00"+bucket]}, nil
}

func (m *mockManifests) ListVersions(_ context.Context, _, _, objectKeyHash string) ([]*metadata.ObjectManifest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.versions[objectKeyHash], nil
}

func (m *mockManifests) Put(_ context.Context, _ manifest_store.ManifestKey, mf *metadata.ObjectManifest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.puts = append(m.puts, mf)
	return nil
}

func (m *mockManifests) Delete(_ context.Context, key manifest_store.ManifestKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deletes = append(m.deletes, key)
	return nil
}

type mockUploads struct {
	byBucket map[string][]*multipart.Upload // key: tenant\x00bucket
	aborted  []string
	parts    []multipart.Part
}

func (m *mockUploads) List(tenantID, bucket string) []*multipart.Upload {
	return m.byBucket[tenantID+"\x00"+bucket]
}

func (m *mockUploads) Abort(uploadID, _ string) (*multipart.Upload, []multipart.Part, error) {
	m.aborted = append(m.aborted, uploadID)
	return nil, m.parts, nil
}

// mockContentIndex scripts a post-decrement count plus optional
// DecrementRef/Delete errors and records the calls, so a test can
// assert the evaluator's refcount-aware reclamation order.
type mockContentIndex struct {
	newCount    int
	decErr      error
	delErr      error
	decremented []string
	rowsDeleted []string
}

func (m *mockContentIndex) DecrementRef(_ context.Context, _, contentHash string) (int, error) {
	m.decremented = append(m.decremented, contentHash)
	return m.newCount, m.decErr
}

func (m *mockContentIndex) Delete(_ context.Context, _, contentHash string) error {
	if m.delErr != nil {
		return m.delErr
	}
	m.rowsDeleted = append(m.rowsDeleted, contentHash)
	return nil
}

// fakeProvider records DeletePiece calls and stubs the rest of the
// providers.StorageProvider interface.
type fakeProvider struct {
	deleted []string
	country string
}

func (f *fakeProvider) PutPiece(context.Context, string, io.Reader, providers.PutOptions) (providers.PutResult, error) {
	return providers.PutResult{}, nil
}
func (f *fakeProvider) GetPiece(context.Context, string, *providers.ByteRange) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeProvider) HeadPiece(context.Context, string) (providers.PieceMetadata, error) {
	return providers.PieceMetadata{}, nil
}
func (f *fakeProvider) DeletePiece(_ context.Context, pieceID string) error {
	f.deleted = append(f.deleted, pieceID)
	return nil
}
func (f *fakeProvider) ListPieces(context.Context, string, string) (providers.ListResult, error) {
	return providers.ListResult{}, nil
}
func (f *fakeProvider) Capabilities() providers.ProviderCapabilities {
	return providers.ProviderCapabilities{}
}
func (f *fakeProvider) CostModel() providers.ProviderCostModel { return providers.ProviderCostModel{} }
func (f *fakeProvider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{Country: f.country}
}

// mockAudit records every lifecycle audit entry. When err is non-nil
// Record returns it (without recording) so the logging path can be
// exercised.
type mockAudit struct {
	entries []AuditEntry
	err     error
}

func (m *mockAudit) Record(_ context.Context, e AuditEntry) error {
	if m.err != nil {
		return m.err
	}
	m.entries = append(m.entries, e)
	return nil
}

// mockBilling records every usage event.
type mockBilling struct {
	events []billing.UsageEvent
}

func (m *mockBilling) Emit(e billing.UsageEvent) { m.events = append(m.events, e) }

func (m *mockBilling) countOf(dim billing.Dimension) int {
	n := 0
	for _, e := range m.events {
		if e.Dimension == dim {
			n++
		}
	}
	return n
}

// ---- helpers ----

const (
	tnt = "tenant-1"
	bkt = "bucket-1"
)

func vkey(t, b string) string { return t + "\x00" + b }

func enabledExpireRule(prefix string, days int) lifecycle.Rule {
	return lifecycle.Rule{
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Prefix: prefix},
		Expiration: &lifecycle.Expiration{Days: days},
	}
}

func entry(rules ...lifecycle.Rule) bucket_config.LifecycleEntry {
	return bucket_config.LifecycleEntry{
		TenantID: tnt,
		Bucket:   bkt,
		Config:   lifecycle.Config{Rules: rules},
	}
}

func fixedClock(t time.Time) func() time.Time { return func() time.Time { return t } }

// ---- tests ----

func TestRun_RequiresSourceAndManifests(t *testing.T) {
	e := New(Config{})
	if _, err := e.Run(context.Background()); err == nil {
		t.Fatal("expected error when Source and Manifests are nil")
	}
}

func TestRun_ListError(t *testing.T) {
	src := &mockSource{listErr: context.DeadlineExceeded}
	e := New(Config{Source: src, Manifests: newMockManifests()})
	if _, err := e.Run(context.Background()); err == nil {
		t.Fatal("expected fatal error when ListLifecycle fails")
	}
}

func TestRun_ExpireUnversionedPermanentDelete(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(0, 0, -40) // 40 days old, rule = 30

	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: created,
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}

	prov := &fakeProvider{}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	e := New(Config{
		Source:    src,
		Manifests: mans,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ObjectsExpired != 1 {
		t.Fatalf("ObjectsExpired = %d, want 1 (%+v)", stats.ObjectsExpired, stats)
	}
	if stats.DeleteMarkersCreated != 0 {
		t.Fatalf("DeleteMarkersCreated = %d, want 0", stats.DeleteMarkersCreated)
	}
	if len(mans.deletes) != 1 || mans.deletes[0].VersionID != "v1" {
		t.Fatalf("expected one delete of v1; got %+v", mans.deletes)
	}
	if len(prov.deleted) != 1 || prov.deleted[0] != "p1" {
		t.Fatalf("expected piece p1 deleted; got %+v", prov.deleted)
	}
}

// TestRun_ExpireDedupRefcountAware verifies the evaluator mirrors the
// interactive DELETE path when permanently expiring a deduped object:
// it decrements the shared content_index refcount and only removes the
// canonical backend piece (after dropping the index row) on the final
// reference — never deleting a piece another object still shares.
func TestRun_ExpireDedupRefcountAware(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(0, 0, -40) // 40 days old, rule = 30

	newManifest := func() *metadata.ObjectManifest {
		return &metadata.ObjectManifest{
			TenantID: tnt, Bucket: bkt,
			ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
			CreatedAt:   created,
			ContentHash: "blake3:abc",
			Pieces:      []metadata.Piece{{PieceID: "p1", Backend: "test"}},
		}
	}

	cases := []struct {
		name         string
		idx          *mockContentIndex
		wantDecr     bool
		wantRowDel   bool
		wantPieceDel bool
	}{
		{
			name:         "shared piece kept while other refs remain",
			idx:          &mockContentIndex{newCount: 2},
			wantDecr:     true,
			wantRowDel:   false,
			wantPieceDel: false,
		},
		{
			name:         "last reference drops row then piece",
			idx:          &mockContentIndex{newCount: 0},
			wantDecr:     true,
			wantRowDel:   true,
			wantPieceDel: true,
		},
		{
			name:         "concurrent re-ref skips piece delete",
			idx:          &mockContentIndex{newCount: 0, delErr: content_index.ErrRefCountNonZero},
			wantDecr:     true,
			wantRowDel:   false,
			wantPieceDel: false,
		},
		{
			name:         "missing index row falls back to best-effort piece delete",
			idx:          &mockContentIndex{decErr: content_index.ErrNotFound},
			wantDecr:     true,
			wantRowDel:   false,
			wantPieceDel: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf := newManifest()
			mans := newMockManifests()
			mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
			prov := &fakeProvider{}
			src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

			e := New(Config{
				Source:       src,
				Manifests:    mans,
				Providers:    map[string]providers.StorageProvider{"test": prov},
				ContentIndex: tc.idx,
				Clock:        fixedClock(now),
			})
			stats, err := e.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// The manifest is always removed regardless of piece fate.
			if stats.ObjectsExpired != 1 || len(mans.deletes) != 1 {
				t.Fatalf("manifest must be deleted; ObjectsExpired=%d deletes=%+v", stats.ObjectsExpired, mans.deletes)
			}
			if got := len(tc.idx.decremented) > 0; got != tc.wantDecr {
				t.Fatalf("DecrementRef called=%v, want %v", got, tc.wantDecr)
			}
			if got := len(tc.idx.rowsDeleted) > 0; got != tc.wantRowDel {
				t.Fatalf("index row deleted=%v, want %v", got, tc.wantRowDel)
			}
			if got := len(prov.deleted) > 0; got != tc.wantPieceDel {
				t.Fatalf("backend piece deleted=%v (%+v), want %v", got, prov.deleted, tc.wantPieceDel)
			}
		})
	}
}

func TestRun_ExpireVersionedInsertsDeleteMarker(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(0, 0, -40)

	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: created,
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}

	prov := &fakeProvider{}
	src := &mockSource{
		entries:    []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))},
		versioning: map[string]bucket_config.VersioningState{vkey(tnt, bkt): bucket_config.VersioningEnabled},
	}

	e := New(Config{
		Source:       src,
		Manifests:    mans,
		Providers:    map[string]providers.StorageProvider{"test": prov},
		Clock:        fixedClock(now),
		NewVersionID: func(_, _, _ string, _ time.Time) string { return "marker-1" },
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.DeleteMarkersCreated != 1 || stats.ObjectsExpired != 0 {
		t.Fatalf("want 1 marker / 0 permanent; got %+v", stats)
	}
	if len(mans.deletes) != 0 {
		t.Fatalf("versioned expiration must not delete the version; got %+v", mans.deletes)
	}
	if len(prov.deleted) != 0 {
		t.Fatalf("versioned expiration must not delete pieces; got %+v", prov.deleted)
	}
	if len(mans.puts) != 1 || !mans.puts[0].DeleteMarker || mans.puts[0].VersionID != "marker-1" {
		t.Fatalf("expected a delete-marker put; got %+v", mans.puts)
	}
}

func TestExpire_ObjectLockSkipsPermanentDelete(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	created := now.AddDate(0, 0, -40)

	cases := []struct {
		name string
		mut  func(*metadata.ObjectManifest)
	}{
		{"legal hold", func(m *metadata.ObjectManifest) { m.LegalHold = true }},
		{"retention", func(m *metadata.ObjectManifest) { m.RetainUntil = now.AddDate(0, 0, 10) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mf := &metadata.ObjectManifest{
				TenantID: tnt, Bucket: bkt,
				ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
				CreatedAt: created,
				Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
			}
			tc.mut(mf)
			mans := newMockManifests()
			mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
			src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

			e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
			stats, err := e.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if stats.Skipped != 1 || stats.ObjectsExpired != 0 {
				t.Fatalf("want skipped=1 expired=0; got %+v", stats)
			}
			if len(mans.deletes) != 0 {
				t.Fatalf("locked object must not be deleted; got %+v", mans.deletes)
			}
		})
	}
}

func TestExpire_UnknownAgeAndNotYetExpired(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	unknown := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/old.txt", ObjectKeyHash: "h1", VersionID: "v1",
		// CreatedAt zero → unknown age → never expires.
	}
	young := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/new.txt", ObjectKeyHash: "h2", VersionID: "v2",
		CreatedAt: now.AddDate(0, 0, -5), // younger than 30 days
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{unknown, young}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ObjectsScanned != 2 {
		t.Fatalf("ObjectsScanned = %d, want 2", stats.ObjectsScanned)
	}
	if stats.ObjectsExpired != 0 || len(mans.deletes) != 0 {
		t.Fatalf("nothing should expire; got %+v deletes=%+v", stats, mans.deletes)
	}
}

func TestExpire_PrefixMismatchSkipped(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "data/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	// Rule only matches logs/, object is data/.
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.ObjectsExpired != 0 || len(mans.deletes) != 0 {
		t.Fatalf("prefix mismatch must not expire; got %+v", stats)
	}
}

func TestExpire_DisabledRuleIgnored(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
	}
	rule := enabledExpireRule("logs/", 30)
	rule.Status = lifecycle.StatusDisabled
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.ObjectsExpired != 0 || len(mans.deletes) != 0 {
		t.Fatalf("disabled rule must not expire; got %+v", stats)
	}
}

func TestExpire_DateBasedExpiration(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -1), // age irrelevant for Date rule
	}
	rule := lifecycle.Rule{
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Prefix: "logs/"},
		Expiration: &lifecycle.Expiration{Date: now.AddDate(0, 0, -1)}, // cutoff in the past
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.ObjectsExpired != 1 {
		t.Fatalf("date-based expiration should fire; got %+v", stats)
	}
}

func TestExpire_DeleteMarkerNotExpiredByAge(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	marker := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		DeleteMarker: true,
		CreatedAt:    now.AddDate(0, 0, -40),
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{marker}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.ObjectsExpired != 0 || stats.DeleteMarkersRemoved != 0 || len(mans.deletes) != 0 {
		t.Fatalf("an age rule must not touch a delete marker; got %+v", stats)
	}
}

func TestExpiredObjectDeleteMarker_RemovesSoleMarker(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	marker := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		DeleteMarker: true,
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{marker}
	mans.versions["h1"] = []*metadata.ObjectManifest{marker} // sole version

	rule := lifecycle.Rule{
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Prefix: "logs/"},
		Expiration: &lifecycle.Expiration{ExpiredObjectDeleteMarker: true},
	}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.DeleteMarkersRemoved != 1 {
		t.Fatalf("sole expired marker should be removed; got %+v", stats)
	}
	if len(mans.deletes) != 1 || mans.deletes[0].VersionID != "v1" {
		t.Fatalf("expected delete of marker v1; got %+v", mans.deletes)
	}
}

func TestExpiredObjectDeleteMarker_KeepsMarkerWithVersionsBehind(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	marker := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v2",
		DeleteMarker: true,
	}
	older := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt, ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{marker}
	mans.versions["h1"] = []*metadata.ObjectManifest{marker, older} // versions remain

	rule := lifecycle.Rule{
		Status:     lifecycle.StatusEnabled,
		Filter:     lifecycle.Filter{Prefix: "logs/"},
		Expiration: &lifecycle.Expiration{ExpiredObjectDeleteMarker: true},
	}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}

	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	stats, _ := e.Run(context.Background())
	if stats.DeleteMarkersRemoved != 0 || len(mans.deletes) != 0 {
		t.Fatalf("marker with versions behind must be kept; got %+v deletes=%+v", stats, mans.deletes)
	}
}

func TestAbortStaleUploads(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	stale := &multipart.Upload{ID: "u-stale", TenantID: tnt, Bucket: bkt, ObjectKey: "uploads/big", CreatedAt: now.AddDate(0, 0, -10)}
	fresh := &multipart.Upload{ID: "u-fresh", TenantID: tnt, Bucket: bkt, ObjectKey: "uploads/small", CreatedAt: now.AddDate(0, 0, -1)}
	otherPrefix := &multipart.Upload{ID: "u-other", TenantID: tnt, Bucket: bkt, ObjectKey: "data/x", CreatedAt: now.AddDate(0, 0, -10)}

	ups := &mockUploads{
		byBucket: map[string][]*multipart.Upload{vkey(tnt, bkt): {stale, fresh, otherPrefix}},
		parts:    []multipart.Part{{PieceID: "pp1", Backend: "test"}},
	}
	prov := &fakeProvider{}

	rule := lifecycle.Rule{
		Status:                         lifecycle.StatusEnabled,
		Filter:                         lifecycle.Filter{Prefix: "uploads/"},
		AbortIncompleteMultipartUpload: &lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
	}
	mans := newMockManifests()
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}

	e := New(Config{
		Source:    src,
		Manifests: mans,
		Uploads:   ups,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.UploadsAborted != 1 {
		t.Fatalf("UploadsAborted = %d, want 1 (%+v)", stats.UploadsAborted, stats)
	}
	if len(ups.aborted) != 1 || ups.aborted[0] != "u-stale" {
		t.Fatalf("expected only u-stale aborted; got %+v", ups.aborted)
	}
	if len(prov.deleted) != 1 || prov.deleted[0] != "pp1" {
		t.Fatalf("expected staged part pp1 reclaimed; got %+v", prov.deleted)
	}
}

func TestAbortStaleUploads_NilUploadsNoop(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	rule := lifecycle.Rule{
		Status:                         lifecycle.StatusEnabled,
		AbortIncompleteMultipartUpload: &lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
	}
	mans := newMockManifests()
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(rule)}}
	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run with nil Uploads should be a no-op, got %v", err)
	}
}

func TestRun_ContextCancellation(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}
	mans := newMockManifests()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e := New(Config{Source: src, Manifests: mans, Clock: fixedClock(now)})
	if _, err := e.Run(ctx); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

// ---- audit & billing wiring ----

// TestAudit_PermanentExpiration verifies a permanent (unversioned)
// expiration emits a LifecycleExpiration audit entry carrying the
// representative piece's backend + resolved placement country, and a
// single LifecycleExpirations billing event — mirroring the
// interactive DELETE path.
func TestAudit_PermanentExpiration(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	prov := &fakeProvider{country: "SG"}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}
	aud := &mockAudit{}
	bill := &mockBilling{}

	e := New(Config{
		Source:    src,
		Manifests: mans,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
		Audit:     aud,
		Billing:   bill,
		NodeID:    "node-7",
	})
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(aud.entries) != 1 {
		t.Fatalf("want 1 audit entry; got %+v", aud.entries)
	}
	got := aud.entries[0]
	if got.Operation != auditOpExpiration {
		t.Errorf("Operation = %q, want %q", got.Operation, auditOpExpiration)
	}
	if got.TenantID != tnt || got.Bucket != bkt || got.ObjectKey != "logs/a.txt" {
		t.Errorf("entry identity = %+v", got)
	}
	if got.PieceID != "p1" || got.PieceBackend != "test" || got.BackendCountry != "SG" {
		t.Errorf("piece fields = %q/%q/%q, want p1/test/SG", got.PieceID, got.PieceBackend, got.BackendCountry)
	}
	if !got.Timestamp.Equal(now) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, now)
	}
	if got.RequestID != "" {
		t.Errorf("RequestID = %q, want empty (no inbound request)", got.RequestID)
	}

	if bill.countOf(billing.LifecycleExpirations) != 1 {
		t.Errorf("want 1 LifecycleExpirations event; got %+v", bill.events)
	}
	if bill.countOf(billing.DeleteRequests) != 0 {
		t.Errorf("lifecycle expiration must NOT emit a Tier-1 DeleteRequests event; got %+v", bill.events)
	}
	if len(bill.events) > 0 && bill.events[0].SourceNodeID != "node-7" {
		t.Errorf("SourceNodeID = %q, want node-7", bill.events[0].SourceNodeID)
	}
}

// TestAudit_DedupExpirationEmitsRefcountSample verifies a deduped
// expiration that leaves other references behind emits the
// post-decrement DedupRefCount sample (Delta = the new count), exactly
// like the interactive DELETE path.
func TestAudit_DedupExpirationEmitsRefcountSample(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt:   now.AddDate(0, 0, -40),
		ContentHash: "blake3:abc",
		Pieces:      []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	prov := &fakeProvider{}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}
	bill := &mockBilling{}

	e := New(Config{
		Source:       src,
		Manifests:    mans,
		Providers:    map[string]providers.StorageProvider{"test": prov},
		ContentIndex: &mockContentIndex{newCount: 2},
		Clock:        fixedClock(now),
		Billing:      bill,
	})
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if bill.countOf(billing.LifecycleExpirations) != 1 {
		t.Errorf("want 1 LifecycleExpirations event; got %+v", bill.events)
	}
	var sample *billing.UsageEvent
	for i := range bill.events {
		if bill.events[i].Dimension == billing.DedupRefCount {
			sample = &bill.events[i]
		}
	}
	if sample == nil {
		t.Fatalf("want a DedupRefCount sample; got %+v", bill.events)
	}
	if sample.Delta != 2 {
		t.Errorf("DedupRefCount Delta = %d, want 2 (post-decrement count)", sample.Delta)
	}
}

// TestAudit_DeleteMarkerAndAbort verifies the versioned-expiration and
// multipart-abort paths each emit their audit entry and dedicated
// billing dimension.
func TestAudit_DeleteMarkerAndAbort(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)

	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}

	stale := &multipart.Upload{ID: "u-stale", TenantID: tnt, Bucket: bkt, ObjectKey: "uploads/big", CreatedAt: now.AddDate(0, 0, -10)}
	ups := &mockUploads{
		byBucket: map[string][]*multipart.Upload{vkey(tnt, bkt): {stale}},
		parts:    []multipart.Part{{PieceID: "pp1", Backend: "test"}},
	}
	prov := &fakeProvider{country: "DE"}

	rule := lifecycle.Rule{
		Status:                         lifecycle.StatusEnabled,
		Filter:                         lifecycle.Filter{Prefix: ""},
		Expiration:                     &lifecycle.Expiration{Days: 30},
		AbortIncompleteMultipartUpload: &lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
	}
	src := &mockSource{
		entries:    []bucket_config.LifecycleEntry{entry(rule)},
		versioning: map[string]bucket_config.VersioningState{vkey(tnt, bkt): bucket_config.VersioningEnabled},
	}
	aud := &mockAudit{}
	bill := &mockBilling{}

	e := New(Config{
		Source:       src,
		Manifests:    mans,
		Uploads:      ups,
		Providers:    map[string]providers.StorageProvider{"test": prov},
		Clock:        fixedClock(now),
		NewVersionID: func(_, _, _ string, _ time.Time) string { return "marker-1" },
		Audit:        aud,
		Billing:      bill,
	})
	if _, err := e.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	ops := map[string]int{}
	for _, en := range aud.entries {
		ops[en.Operation]++
	}
	if ops[auditOpDeleteMarker] != 1 {
		t.Errorf("want 1 %s audit entry; got %+v", auditOpDeleteMarker, aud.entries)
	}
	if ops[auditOpAbortMultipart] != 1 {
		t.Errorf("want 1 %s audit entry; got %+v", auditOpAbortMultipart, aud.entries)
	}
	if bill.countOf(billing.LifecycleDeleteMarkers) != 1 {
		t.Errorf("want 1 LifecycleDeleteMarkers event; got %+v", bill.events)
	}
	if bill.countOf(billing.LifecycleAbortedUploads) != 1 {
		t.Errorf("want 1 LifecycleAbortedUploads event; got %+v", bill.events)
	}
	if bill.countOf(billing.LifecycleExpirations) != 0 {
		t.Errorf("versioned expiration must not emit LifecycleExpirations; got %+v", bill.events)
	}
}

// TestAudit_NilHooksNoop verifies that with neither hook wired the
// evaluator still performs its actions without panicking.
func TestAudit_NilHooksNoop(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	prov := &fakeProvider{}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	e := New(Config{
		Source:    src,
		Manifests: mans,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
		// Audit and Billing intentionally nil.
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ObjectsExpired != 1 {
		t.Fatalf("expiration must still run with nil hooks; got %+v", stats)
	}
}

// TestAudit_RecordErrorIsLogged verifies a failed audit recording is
// surfaced via the logger (not silently dropped) and the expiration
// still completes.
func TestAudit_RecordErrorIsLogged(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/a.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
		Pieces:    []metadata.Piece{{PieceID: "p1", Backend: "test"}},
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	prov := &fakeProvider{}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}

	var logBuf bytes.Buffer
	e := New(Config{
		Source:    src,
		Manifests: mans,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
		Audit:     &mockAudit{err: errors.New("audit store down")},
		Logger:    log.New(&logBuf, "", 0),
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ObjectsExpired != 1 {
		t.Fatalf("expiration must still complete when audit fails; got %+v", stats)
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "audit") || !strings.Contains(logged, "audit store down") {
		t.Errorf("audit failure must be logged; got %q", logged)
	}
}

// TestAudit_PieceLessExpirationSkipsAudit verifies a piece-less
// manifest expiration (e.g. a zero-byte object) is still metered but
// produces no audit entry, matching the interactive DELETE path which
// only audits when len(manifest.Pieces) > 0.
func TestAudit_PieceLessExpirationSkipsAudit(t *testing.T) {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	mf := &metadata.ObjectManifest{
		TenantID: tnt, Bucket: bkt,
		ObjectKey: "logs/empty.txt", ObjectKeyHash: "h1", VersionID: "v1",
		CreatedAt: now.AddDate(0, 0, -40),
		// No Pieces: a zero-byte object.
	}
	mans := newMockManifests()
	mans.latest[vkey(tnt, bkt)] = []*metadata.ObjectManifest{mf}
	prov := &fakeProvider{}
	src := &mockSource{entries: []bucket_config.LifecycleEntry{entry(enabledExpireRule("logs/", 30))}}
	aud := &mockAudit{}
	bill := &mockBilling{}

	e := New(Config{
		Source:    src,
		Manifests: mans,
		Providers: map[string]providers.StorageProvider{"test": prov},
		Clock:     fixedClock(now),
		Audit:     aud,
		Billing:   bill,
	})
	stats, err := e.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.ObjectsExpired != 1 {
		t.Fatalf("piece-less object must still expire; got %+v", stats)
	}
	if len(aud.entries) != 0 {
		t.Errorf("piece-less expiration must not audit; got %+v", aud.entries)
	}
	if bill.countOf(billing.LifecycleExpirations) != 1 {
		t.Errorf("piece-less expiration must still meter LifecycleExpirations; got %+v", bill.events)
	}
}
