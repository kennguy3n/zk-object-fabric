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
