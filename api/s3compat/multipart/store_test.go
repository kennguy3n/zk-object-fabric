package multipart

import (
	"errors"
	"testing"
	"time"
)

func TestMemoryStore_CreatePutComplete(t *testing.T) {
	store := NewMemoryStore()
	upload := &Upload{
		ID:        "u1",
		TenantID:  "t1",
		Bucket:    "b1",
		ObjectKey: "k1",
		Backend:   "local_fs_dev",
		CreatedAt: time.Unix(1, 0),
	}
	if err := store.Create(upload); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.PutPart("u1", Part{PartNumber: 2, PieceID: "p2", ETag: "e2", SizeBytes: 10}); err != nil {
		t.Fatalf("PutPart 2: %v", err)
	}
	if err := store.PutPart("u1", Part{PartNumber: 1, PieceID: "p1", ETag: "e1", SizeBytes: 20}); err != nil {
		t.Fatalf("PutPart 1: %v", err)
	}

	parts, final, err := store.Complete("u1", []PartReference{
		{PartNumber: 1, ETag: "e1"},
		{PartNumber: 2, ETag: "e2"},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(parts) != 2 || parts[0].PartNumber != 1 || parts[1].PartNumber != 2 {
		t.Fatalf("parts returned out of order: %+v", parts)
	}
	if final.ObjectKey != "k1" {
		t.Fatalf("unexpected final upload: ObjectKey=%q", final.ObjectKey)
	}
	if _, err := store.Get("u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound after complete, got %v", err)
	}
}

func TestMemoryStore_CompleteETagMismatch(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Create(&Upload{ID: "u1", TenantID: "t", Bucket: "b", ObjectKey: "k", Backend: "x"})
	_ = store.PutPart("u1", Part{PartNumber: 1, PieceID: "p1", ETag: "actual"})
	_, _, err := store.Complete("u1", []PartReference{{PartNumber: 1, ETag: "wrong"}})
	if !errors.Is(err, ErrPartETagMismatch) {
		t.Fatalf("expected ErrPartETagMismatch, got %v", err)
	}
	// Upload must still be live so the client can retry.
	if _, err := store.Get("u1"); err != nil {
		t.Fatalf("upload must survive a bad complete: %v", err)
	}
}

func TestMemoryStore_CompleteMissingPart(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Create(&Upload{ID: "u1", TenantID: "t", Bucket: "b", ObjectKey: "k", Backend: "x"})
	_ = store.PutPart("u1", Part{PartNumber: 1, PieceID: "p1", ETag: "e1"})
	_, _, err := store.Complete("u1", []PartReference{
		{PartNumber: 1, ETag: "e1"},
		{PartNumber: 2, ETag: "e2"},
	})
	if !errors.Is(err, ErrPartNotFound) {
		t.Fatalf("expected ErrPartNotFound, got %v", err)
	}
	if _, err := store.Get("u1"); err != nil {
		t.Fatalf("upload must survive a missing-part complete: %v", err)
	}
}

func TestMemoryStore_AbortReturnsParts(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Create(&Upload{ID: "u1", TenantID: "t", Bucket: "b", ObjectKey: "k", Backend: "x"})
	_ = store.PutPart("u1", Part{PartNumber: 1, PieceID: "p1"})
	_ = store.PutPart("u1", Part{PartNumber: 2, PieceID: "p2"})
	_, parts, err := store.Abort("u1")
	if err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts from abort, got %d", len(parts))
	}
	if _, err := store.Get("u1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected upload to be gone after abort: %v", err)
	}
}

func TestMemoryStore_PutPartOverwrite(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Create(&Upload{ID: "u1"})
	_ = store.PutPart("u1", Part{PartNumber: 1, PieceID: "first", ETag: "1"})
	_ = store.PutPart("u1", Part{PartNumber: 1, PieceID: "second", ETag: "2"})
	u, err := store.Get("u1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if u.Parts()[1].PieceID != "second" {
		t.Fatalf("re-uploaded part should win: %+v", u.Parts()[1])
	}
}

func TestMemoryStore_ListScope(t *testing.T) {
	store := NewMemoryStore()
	_ = store.Create(&Upload{ID: "u1", TenantID: "t", Bucket: "b1", CreatedAt: time.Unix(1, 0)})
	_ = store.Create(&Upload{ID: "u2", TenantID: "t", Bucket: "b1", CreatedAt: time.Unix(2, 0)})
	_ = store.Create(&Upload{ID: "u3", TenantID: "t", Bucket: "b2", CreatedAt: time.Unix(3, 0)})
	_ = store.Create(&Upload{ID: "u4", TenantID: "other", Bucket: "b1", CreatedAt: time.Unix(4, 0)})

	b1 := store.List("t", "b1")
	if len(b1) != 2 || b1[0].ID != "u1" || b1[1].ID != "u2" {
		t.Fatalf("unexpected list for (t,b1): %+v", b1)
	}
	all := store.List("t", "")
	if len(all) != 3 {
		t.Fatalf("expected 3 uploads for tenant t, got %d", len(all))
	}
}
