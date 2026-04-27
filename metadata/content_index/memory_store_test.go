package content_index

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestMemoryStore_RegisterLookupIncrementDelete(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	if _, err := s.Lookup(ctx, "tnt", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup empty: got %v want ErrNotFound", err)
	}

	if err := s.Register(ctx, ContentIndexEntry{
		TenantID: "tnt", ContentHash: "h1", PieceID: "p1", Backend: "wasabi", SizeBytes: 1024,
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := s.Lookup(ctx, "tnt", "h1")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.RefCount != 1 || got.PieceID != "p1" || got.SizeBytes != 1024 {
		t.Fatalf("Lookup unexpected entry: %+v", got)
	}

	// Duplicate Register fails.
	if err := s.Register(ctx, ContentIndexEntry{
		TenantID: "tnt", ContentHash: "h1", PieceID: "p1", Backend: "wasabi",
	}); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("Register dup: got %v want ErrAlreadyExists", err)
	}

	// Cross-tenant lookup must fail even when the content_hash matches.
	if _, err := s.Lookup(ctx, "tnt-other", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup cross-tenant: got %v want ErrNotFound", err)
	}

	if err := s.IncrementRef(ctx, "tnt", "h1"); err != nil {
		t.Fatalf("IncrementRef: %v", err)
	}
	got, _ = s.Lookup(ctx, "tnt", "h1")
	if got.RefCount != 2 {
		t.Fatalf("RefCount after Increment = %d want 2", got.RefCount)
	}

	n, err := s.DecrementRef(ctx, "tnt", "h1")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 1 {
		t.Fatalf("DecrementRef = %d want 1", n)
	}
	n, err = s.DecrementRef(ctx, "tnt", "h1")
	if err != nil {
		t.Fatalf("DecrementRef: %v", err)
	}
	if n != 0 {
		t.Fatalf("DecrementRef final = %d want 0", n)
	}
	if _, err := s.DecrementRef(ctx, "tnt", "h1"); !errors.Is(err, ErrInvalidRefCount) {
		t.Fatalf("DecrementRef below 0: got %v want ErrInvalidRefCount", err)
	}

	if err := s.Delete(ctx, "tnt", "h1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Lookup(ctx, "tnt", "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Lookup after Delete: got %v want ErrNotFound", err)
	}
}

func TestMemoryStore_ConcurrentIncrementRef(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.Register(ctx, ContentIndexEntry{
		TenantID: "tnt", ContentHash: "h", PieceID: "p", Backend: "wasabi",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	var wg sync.WaitGroup
	const N = 100
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.IncrementRef(ctx, "tnt", "h")
		}()
	}
	wg.Wait()

	got, _ := s.Lookup(ctx, "tnt", "h")
	if got.RefCount != 1+N {
		t.Fatalf("RefCount = %d want %d", got.RefCount, 1+N)
	}
}

// TestMemoryStore_DeleteRefCountNonZero asserts the conditional-Delete
// contract that the s3compat DELETE handler relies on to close the
// race against concurrent IncrementRefs: a Delete on an entry whose
// RefCount is still > 0 must return ErrRefCountNonZero (not silently
// succeed), so the handler can leave the backend piece in place.
func TestMemoryStore_DeleteRefCountNonZero(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.Register(ctx, ContentIndexEntry{
		TenantID:    "tnt",
		ContentHash: "h1",
		PieceID:     "p1",
		Backend:     "test",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := s.Delete(ctx, "tnt", "h1"); !errors.Is(err, ErrRefCountNonZero) {
		t.Fatalf("Delete with refcount=1: got %v want ErrRefCountNonZero", err)
	}
	if _, err := s.Lookup(ctx, "tnt", "h1"); err != nil {
		t.Fatalf("entry must still exist after refused Delete: %v", err)
	}
}

// TestMemoryStore_PieceIDsRoundTrip asserts the multi-piece
// extension survives Register → Lookup → ScanAll without losing
// or aliasing the per-part metadata. The slice copy in
// clonePieceRefs prevents callers from mutating the store's
// internal state through a returned reference.
func TestMemoryStore_PieceIDsRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	refs := []PieceRef{
		{PieceID: "p1", Backend: "wasabi", PartNumber: 1, SizeBytes: 5 * 1024 * 1024},
		{PieceID: "p2", Backend: "wasabi", PartNumber: 2, SizeBytes: 5 * 1024 * 1024},
		{PieceID: "p3", Backend: "wasabi", PartNumber: 3, SizeBytes: 1024},
	}
	if err := s.Register(ctx, ContentIndexEntry{
		TenantID:    "tnt",
		ContentHash: "multi-h",
		PieceID:     "p1",
		Backend:     "wasabi",
		SizeBytes:   refs[0].SizeBytes + refs[1].SizeBytes + refs[2].SizeBytes,
		PieceIDs:    refs,
	}); err != nil {
		t.Fatalf("Register multi-piece: %v", err)
	}

	got, err := s.Lookup(ctx, "tnt", "multi-h")
	if err != nil {
		t.Fatalf("Lookup multi-piece: %v", err)
	}
	if len(got.PieceIDs) != len(refs) {
		t.Fatalf("Lookup PieceIDs len = %d want %d", len(got.PieceIDs), len(refs))
	}
	for i, want := range refs {
		if got.PieceIDs[i] != want {
			t.Fatalf("Lookup PieceIDs[%d] = %+v want %+v", i, got.PieceIDs[i], want)
		}
	}
	// Mutating the returned slice must not affect the store.
	got.PieceIDs[0].PieceID = "tampered"
	again, _ := s.Lookup(ctx, "tnt", "multi-h")
	if again.PieceIDs[0].PieceID != "p1" {
		t.Fatalf("Lookup after mutation = %q want %q (slice not cloned)", again.PieceIDs[0].PieceID, "p1")
	}

	// ScanAll must surface the full slice too, since orphan GC
	// uses it to enumerate all canonical pieces under a tenant.
	scanned, err := s.ScanAll(ctx, "tnt")
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(scanned) != 1 {
		t.Fatalf("ScanAll len = %d want 1", len(scanned))
	}
	if len(scanned[0].PieceIDs) != len(refs) {
		t.Fatalf("ScanAll PieceIDs len = %d want %d", len(scanned[0].PieceIDs), len(refs))
	}
}

// TestMemoryStore_PieceIDsNilForSinglePiece asserts that
// single-piece entries (no PieceIDs at Register) round-trip with
// PieceIDs == nil, not a non-nil empty slice. Downstream code
// (postgres NULLIF, the multipart redirect helper) distinguishes
// the two and a regression here would silently break dedup-hit
// redirection for legacy single-piece entries.
func TestMemoryStore_PieceIDsNilForSinglePiece(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.Register(ctx, ContentIndexEntry{
		TenantID: "tnt", ContentHash: "single-h", PieceID: "p1", Backend: "wasabi",
	}); err != nil {
		t.Fatalf("Register single-piece: %v", err)
	}
	got, err := s.Lookup(ctx, "tnt", "single-h")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.PieceIDs != nil {
		t.Fatalf("PieceIDs = %v want nil for single-piece entry", got.PieceIDs)
	}
}

func TestMemoryStore_RejectsMissingFields(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	if err := s.IncrementRef(ctx, "tnt", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("IncrementRef missing: got %v want ErrNotFound", err)
	}
	if _, err := s.DecrementRef(ctx, "tnt", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DecrementRef missing: got %v want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "tnt", "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing: got %v want ErrNotFound", err)
	}
}
