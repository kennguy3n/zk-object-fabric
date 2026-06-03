package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "ci.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(Config{DB: db})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func entry(tenant, hash string) content_index.ContentIndexEntry {
	return content_index.ContentIndexEntry{
		TenantID:    tenant,
		ContentHash: hash,
		PieceID:     "piece-" + hash,
		Backend:     "wasabi",
		SizeBytes:   1024,
		ETag:        "etag-" + hash,
	}
}

func TestRegisterLookup(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Register(ctx, entry("t1", "h1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := s.Lookup(ctx, "t1", "h1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.PieceID != "piece-h1" || got.Backend != "wasabi" || got.SizeBytes != 1024 || got.ETag != "etag-h1" {
		t.Fatalf("entry mismatch: %+v", got)
	}
	if got.RefCount != 1 {
		t.Fatalf("RefCount = %d, want 1", got.RefCount)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero; want a populated timestamp")
	}

	// Duplicate Register returns ErrAlreadyExists without clobbering.
	if err := s.Register(ctx, entry("t1", "h1")); err != content_index.ErrAlreadyExists {
		t.Fatalf("duplicate Register: err = %v, want ErrAlreadyExists", err)
	}

	// Cross-tenant isolation.
	if _, err := s.Lookup(ctx, "t2", "h1"); err != content_index.ErrNotFound {
		t.Fatalf("cross-tenant Lookup: err = %v, want ErrNotFound", err)
	}
}

func TestRefCounting(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Register(ctx, entry("t1", "h1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.IncrementRef(ctx, "t1", "h1"); err != nil {
		t.Fatalf("IncrementRef: %v", err)
	}
	got, _ := s.Lookup(ctx, "t1", "h1")
	if got.RefCount != 2 {
		t.Fatalf("RefCount = %d, want 2", got.RefCount)
	}

	n, err := s.DecrementRef(ctx, "t1", "h1")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 1 {
		t.Fatalf("DecrementRef returned %d, want 1", n)
	}
	n, err = s.DecrementRef(ctx, "t1", "h1")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 0 {
		t.Fatalf("DecrementRef returned %d, want 0", n)
	}

	// Decrement at zero is a typed programmer-error signal.
	if _, err := s.DecrementRef(ctx, "t1", "h1"); err != content_index.ErrInvalidRefCount {
		t.Fatalf("DecrementRef at zero: err = %v, want ErrInvalidRefCount", err)
	}

	// Missing row.
	if err := s.IncrementRef(ctx, "t1", "missing"); err != content_index.ErrNotFound {
		t.Fatalf("IncrementRef missing: err = %v, want ErrNotFound", err)
	}
	if _, err := s.DecrementRef(ctx, "t1", "missing"); err != content_index.ErrNotFound {
		t.Fatalf("DecrementRef missing: err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Register(ctx, entry("t1", "h1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// ref_count is 1, so Delete must refuse and report non-zero.
	if err := s.Delete(ctx, "t1", "h1"); err != content_index.ErrRefCountNonZero {
		t.Fatalf("Delete with ref_count=1: err = %v, want ErrRefCountNonZero", err)
	}
	if _, err := s.DecrementRef(ctx, "t1", "h1"); err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if err := s.Delete(ctx, "t1", "h1"); err != nil {
		t.Fatalf("Delete at zero: %v", err)
	}
	if err := s.Delete(ctx, "t1", "h1"); err != content_index.ErrNotFound {
		t.Fatalf("Delete missing: err = %v, want ErrNotFound", err)
	}
}

func TestPlaintextHashLookupAndPieceIDs(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	e := entry("t1", "h1")
	e.PlaintextHash = "pt-hash-1"
	e.PieceIDs = []content_index.PieceRef{
		{PieceID: "p1", Backend: "wasabi", PartNumber: 1, SizeBytes: 10},
		{PieceID: "p2", Backend: "wasabi", PartNumber: 2, SizeBytes: 20},
	}
	if err := s.Register(ctx, e); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := s.LookupByPlaintextHash(ctx, "t1", "pt-hash-1")
	if err != nil {
		t.Fatalf("LookupByPlaintextHash: %v", err)
	}
	if len(got.PieceIDs) != 2 || got.PieceIDs[1].PieceID != "p2" {
		t.Fatalf("PieceIDs round-trip failed: %+v", got.PieceIDs)
	}

	// Empty plaintext hash never matches.
	if _, err := s.LookupByPlaintextHash(ctx, "t1", ""); err != content_index.ErrNotFound {
		t.Fatalf("empty plaintext lookup: err = %v, want ErrNotFound", err)
	}
}

func TestScanAllAndListTenants(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Register(ctx, entry("t1", "h1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Register(ctx, entry("t1", "h2")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Register(ctx, entry("t2", "h3")); err != nil {
		t.Fatalf("Register: %v", err)
	}

	rows, err := s.ScanAll(ctx, "t1")
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ScanAll(t1) len = %d, want 2", len(rows))
	}

	tenants, err := s.ListTenants(ctx)
	if err != nil {
		t.Fatalf("ListTenants: %v", err)
	}
	if len(tenants) != 2 || tenants[0] != "t1" || tenants[1] != "t2" {
		t.Fatalf("ListTenants = %v, want [t1 t2]", tenants)
	}
}

// TestConcurrentDecrementRef proves DecrementRef is atomic: N
// concurrent decrements of a row at ref_count=N must drive it to
// exactly 0, with each call returning a distinct new count in
// [0, N-1], exactly one of them observing 0, and none erroring.
// A non-atomic SELECT-then-UPDATE would let two callers read the
// same value and either skip 0 (orphaning the piece) or push
// ref_count below the CHECK floor.
func TestConcurrentDecrementRef(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	const n = 32
	if err := s.Register(ctx, entry("t1", "h1")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for i := 1; i < n; i++ { // Register set ref_count=1; bump to n.
		if err := s.IncrementRef(ctx, "t1", "h1"); err != nil {
			t.Fatalf("IncrementRef: %v", err)
		}
	}

	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		seen  = make(map[int]int)
		errs  []error
		start = make(chan struct{})
	)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := s.DecrementRef(ctx, "t1", "h1")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			seen[got]++
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) != 0 {
		t.Fatalf("DecrementRef errors: %v", errs)
	}
	for v := 0; v < n; v++ {
		if seen[v] != 1 {
			t.Fatalf("new count %d observed %d times, want exactly 1 (seen=%v)", v, seen[v], seen)
		}
	}
	got, err := s.Lookup(ctx, "t1", "h1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.RefCount != 0 {
		t.Fatalf("final RefCount = %d, want 0", got.RefCount)
	}
}
