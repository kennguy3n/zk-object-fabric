package sqlite

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

func newTestStore(t *testing.T, enc manifest_store.BodyEncryptor) *Store {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "manifests.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(Config{DB: db, BodyEncryptor: enc})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func manifestFor(tenant, bucket, hash, version string) *metadata.ObjectManifest {
	return &metadata.ObjectManifest{
		TenantID:      tenant,
		Bucket:        bucket,
		ObjectKey:     "key-" + hash,
		ObjectKeyHash: hash,
		VersionID:     version,
	}
}

func key(tenant, bucket, hash, version string) manifest_store.ManifestKey {
	return manifest_store.ManifestKey{TenantID: tenant, Bucket: bucket, ObjectKeyHash: hash, VersionID: version}
}

func TestPutGetDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	k := key("t1", "b1", "h1", "v1")
	if err := s.Put(ctx, k, manifestFor("t1", "b1", "h1", "v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ObjectKey != "key-h1" {
		t.Fatalf("ObjectKey = %q, want key-h1", got.ObjectKey)
	}

	// Empty version resolves the latest.
	got, err = s.Get(ctx, key("t1", "b1", "h1", ""))
	if err != nil {
		t.Fatalf("Get latest: %v", err)
	}
	if got.VersionID != "v1" {
		t.Fatalf("latest VersionID = %q, want v1", got.VersionID)
	}

	if err := s.Delete(ctx, k); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, k); err != manifest_store.ErrNotFound {
		t.Fatalf("Get after delete: err = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, k); err != manifest_store.ErrNotFound {
		t.Fatalf("Delete missing: err = %v, want ErrNotFound", err)
	}
}

func TestLatestVersionResolution(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	for _, v := range []string{"v1", "v2", "v3"} {
		if err := s.Put(ctx, key("t1", "b1", "h1", v), manifestFor("t1", "b1", "h1", v)); err != nil {
			t.Fatalf("Put %s: %v", v, err)
		}
	}
	got, err := s.Get(ctx, key("t1", "b1", "h1", ""))
	if err != nil {
		t.Fatalf("Get latest: %v", err)
	}
	if got.VersionID != "v3" {
		t.Fatalf("latest = %q, want v3", got.VersionID)
	}

	// Re-Put an older version; it must become latest again.
	if err := s.Put(ctx, key("t1", "b1", "h1", "v1"), manifestFor("t1", "b1", "h1", "v1")); err != nil {
		t.Fatalf("re-Put v1: %v", err)
	}
	got, err = s.Get(ctx, key("t1", "b1", "h1", ""))
	if err != nil {
		t.Fatalf("Get latest after re-put: %v", err)
	}
	if got.VersionID != "v1" {
		t.Fatalf("latest after re-put = %q, want v1", got.VersionID)
	}

	versions, err := s.ListVersions(ctx, "t1", "b1", "h1")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("ListVersions len = %d, want 3", len(versions))
	}
	if versions[0].VersionID != "v1" {
		t.Fatalf("ListVersions[0] = %q, want v1 (most recent)", versions[0].VersionID)
	}

	// Deleting with empty version removes the latest (v1), leaving v3 latest.
	if err := s.Delete(ctx, key("t1", "b1", "h1", "")); err != nil {
		t.Fatalf("Delete latest: %v", err)
	}
	got, err = s.Get(ctx, key("t1", "b1", "h1", ""))
	if err != nil {
		t.Fatalf("Get latest after delete: %v", err)
	}
	if got.VersionID != "v3" {
		t.Fatalf("latest after delete = %q, want v3", got.VersionID)
	}
}

func TestListPaginationAndIsolation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	// Tenant t1: 5 distinct objects, each with two versions.
	hashes := []string{"h1", "h2", "h3", "h4", "h5"}
	for _, h := range hashes {
		if err := s.Put(ctx, key("t1", "b1", h, "v1"), manifestFor("t1", "b1", h, "v1")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		if err := s.Put(ctx, key("t1", "b1", h, "v2"), manifestFor("t1", "b1", h, "v2")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Tenant t2: one object that must never leak into t1 listings.
	if err := s.Put(ctx, key("t2", "b1", "hX", "v1"), manifestFor("t2", "b1", "hX", "v1")); err != nil {
		t.Fatalf("Put t2: %v", err)
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		res, err := s.List(ctx, "t1", "b1", cursor, 2)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, m := range res.Manifests {
			if m.TenantID != "t1" {
				t.Fatalf("cross-tenant leak: got tenant %q", m.TenantID)
			}
			if m.VersionID != "v2" {
				t.Fatalf("List returned non-latest version %q for %q", m.VersionID, m.ObjectKeyHash)
			}
			if seen[m.ObjectKeyHash] {
				t.Fatalf("duplicate object_key_hash %q across pages", m.ObjectKeyHash)
			}
			seen[m.ObjectKeyHash] = true
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(hashes) {
		t.Fatalf("listed %d objects, want %d", len(seen), len(hashes))
	}
}

func TestHasManifestWithPieceID(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	m := manifestFor("t1", "b1", "h1", "v1")
	m.Pieces = []metadata.Piece{{PieceID: "piece-abc"}}
	if err := s.Put(ctx, key("t1", "b1", "h1", "v1"), m); err != nil {
		t.Fatalf("Put: %v", err)
	}
	ok, err := s.HasManifestWithPieceID(ctx, "t1", "piece-abc")
	if err != nil {
		t.Fatalf("HasManifestWithPieceID: %v", err)
	}
	if !ok {
		t.Fatal("expected piece-abc to be found")
	}
	ok, err = s.HasManifestWithPieceID(ctx, "t1", "piece-missing")
	if err != nil {
		t.Fatalf("HasManifestWithPieceID: %v", err)
	}
	if ok {
		t.Fatal("did not expect piece-missing to be found")
	}
	// Cross-tenant: t2 must not see t1's piece.
	ok, err = s.HasManifestWithPieceID(ctx, "t2", "piece-abc")
	if err != nil {
		t.Fatalf("HasManifestWithPieceID t2: %v", err)
	}
	if ok {
		t.Fatal("cross-tenant piece lookup leaked")
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "manifests.db")

	db, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s, err := New(Config{DB: db})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := s.Put(context.Background(), key("t1", "b1", "h1", "v1"), manifestFor("t1", "b1", "h1", "v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen the same file: the manifest must still be there.
	db2, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db2.Close()
	s2, err := New(Config{DB: db2})
	if err != nil {
		t.Fatalf("new (reopen): %v", err)
	}
	got, err := s2.Get(context.Background(), key("t1", "b1", "h1", "v1"))
	if err != nil {
		t.Fatalf("Get after reopen: %v", err)
	}
	if got.ObjectKeyHash != "h1" {
		t.Fatalf("ObjectKeyHash = %q, want h1", got.ObjectKeyHash)
	}
}

func TestBodyEncryptorRoundTrip(t *testing.T) {
	t.Parallel()
	k := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	enc, err := manifest_store.NewAEADBodyEncryptor(k)
	if err != nil {
		t.Fatalf("new encryptor: %v", err)
	}
	s := newTestStore(t, enc)
	ctx := context.Background()

	if err := s.Put(ctx, key("t1", "b1", "h1", "v1"), manifestFor("t1", "b1", "h1", "v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, key("t1", "b1", "h1", "v1"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ObjectKey != "key-h1" {
		t.Fatalf("ObjectKey = %q, want key-h1", got.ObjectKey)
	}
	// List and ListVersions must also decrypt cleanly.
	res, err := s.List(ctx, "t1", "b1", "", 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Manifests) != 1 {
		t.Fatalf("List len = %d, want 1", len(res.Manifests))
	}
}

func TestValidation(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()
	if err := s.Put(ctx, key("", "b", "h", "v"), manifestFor("", "b", "h", "v")); err == nil {
		t.Fatal("Put with empty tenant: want error")
	}
	if err := s.Put(ctx, key("t", "b", "h", ""), manifestFor("t", "b", "h", "")); err == nil {
		t.Fatal("Put with empty version: want error")
	}
	if err := s.Put(ctx, key("t", "b", "h", "v"), nil); err == nil {
		t.Fatal("Put nil manifest: want error")
	}
}

// scanKeyLess is the full-primary-key ordering the SQLite ScanManifests
// query (ORDER BY tenant_id, bucket, object_key_hash, version_id) must
// produce; the test asserts the rows come back strictly increasing.
func scanKeyLess(a, b manifest_store.ManifestKey) bool {
	if a.TenantID != b.TenantID {
		return a.TenantID < b.TenantID
	}
	if a.Bucket != b.Bucket {
		return a.Bucket < b.Bucket
	}
	if a.ObjectKeyHash != b.ObjectKeyHash {
		return a.ObjectKeyHash < b.ObjectKeyHash
	}
	return a.VersionID < b.VersionID
}

// TestScanManifests_PaginatesEveryVersionInKeyOrder mirrors the memory
// store's contract test: ScanManifests must visit every version (not
// just the latest) across all tenants/buckets exactly once, in full
// primary-key order, with a keyset cursor that resumes without gaps or
// repeats. This also exercises the SQLite row-value comparison
// (tenant_id, bucket, object_key_hash, version_id) > (?, ?, ?, ?)
// against the modernc driver.
func TestScanManifests_PaginatesEveryVersionInKeyOrder(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	want := map[manifest_store.ManifestKey]bool{}
	seed := func(tenant, bucket, hash, version string) {
		mk := key(tenant, bucket, hash, version)
		if err := s.Put(ctx, mk, manifestFor(tenant, bucket, hash, version)); err != nil {
			t.Fatalf("seed %v: %v", mk, err)
		}
		want[mk] = true
	}
	seed("t1", "b1", "h1", "v1")
	seed("t1", "b1", "h1", "v2") // older version of same object
	seed("t1", "b1", "h2", "v1")
	seed("t1", "b2", "h1", "v1")
	seed("t2", "b1", "h1", "v1")

	got := map[manifest_store.ManifestKey]int{}
	var prev manifest_store.ManifestKey
	var havePrev bool
	cursor := ""
	pages := 0
	for {
		page, err := s.ScanManifests(ctx, cursor, 2)
		if err != nil {
			t.Fatalf("ScanManifests: %v", err)
		}
		pages++
		for _, sm := range page.Manifests {
			got[sm.Key]++
			if havePrev && !scanKeyLess(prev, sm.Key) {
				t.Fatalf("scan not strictly increasing: %v then %v", prev, sm.Key)
			}
			prev, havePrev = sm.Key, true
			if sm.Manifest == nil {
				t.Fatalf("nil manifest for %v", sm.Key)
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(got) != len(want) {
		t.Fatalf("scanned %d distinct keys, want %d", len(got), len(want))
	}
	for k := range want {
		if got[k] != 1 {
			t.Fatalf("key %v visited %d times, want exactly 1", k, got[k])
		}
	}
	if pages < 2 {
		t.Fatalf("expected multiple pages with limit=2 over %d keys, got %d", len(want), pages)
	}
}

// TestScanManifests_RejectsMalformedCursor ensures a corrupted cursor
// is an error rather than a silent restart from the beginning.
func TestScanManifests_RejectsMalformedCursor(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	if _, err := s.ScanManifests(context.Background(), "!!!not-base64!!!", 10); err == nil {
		t.Fatal("malformed cursor: want error, got nil")
	}
}

// TestUpdateManifest_PreservesLatestPointer pins the contract:
// amending a non-latest version in place must not bump write_seq, so
// the empty-VersionID "latest" read still resolves to the newest Put.
func TestUpdateManifest_PreservesLatestPointer(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	ctx := context.Background()

	if err := s.Put(ctx, key("t1", "b1", "h1", "v1"), manifestFor("t1", "b1", "h1", "v1")); err != nil {
		t.Fatalf("Put v1: %v", err)
	}
	if err := s.Put(ctx, key("t1", "b1", "h1", "v2"), manifestFor("t1", "b1", "h1", "v2")); err != nil {
		t.Fatalf("Put v2: %v", err)
	}

	amended := manifestFor("t1", "b1", "h1", "v1")
	amended.Tags = map[string]string{"env": "prod"}
	if err := s.UpdateManifest(ctx, key("t1", "b1", "h1", "v1"), amended); err != nil {
		t.Fatalf("UpdateManifest v1: %v", err)
	}

	latest, err := s.Get(ctx, key("t1", "b1", "h1", ""))
	if err != nil {
		t.Fatalf("Get latest: %v", err)
	}
	if latest.VersionID != "v2" {
		t.Fatalf("latest VersionID = %q, want v2", latest.VersionID)
	}

	got1, err := s.Get(ctx, key("t1", "b1", "h1", "v1"))
	if err != nil {
		t.Fatalf("Get v1: %v", err)
	}
	if got1.Tags["env"] != "prod" {
		t.Fatalf("v1 Tags = %v, want env=prod", got1.Tags)
	}
}

// TestUpdateManifest_NotFound verifies an amend to a missing version
// returns ErrNotFound rather than inserting a row.
func TestUpdateManifest_NotFound(t *testing.T) {
	t.Parallel()
	s := newTestStore(t, nil)
	err := s.UpdateManifest(context.Background(), key("t1", "b1", "h1", "nope"), manifestFor("t1", "b1", "h1", "nope"))
	if err != manifest_store.ErrNotFound {
		t.Fatalf("UpdateManifest missing key err = %v, want ErrNotFound", err)
	}
}
