package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLegalHold_MatchesScopes(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tenantHold := LegalHold{TenantID: "T", IssuedBy: "ops", Reason: "case", CreatedAt: now}
	bucketHold := LegalHold{TenantID: "T", Bucket: "b", IssuedBy: "ops", Reason: "case", CreatedAt: now}
	objectHold := LegalHold{TenantID: "T", Bucket: "b", ObjectKey: "k", IssuedBy: "ops", Reason: "case", CreatedAt: now}

	cases := []struct {
		hold              LegalHold
		bucket, key       string
		want              bool
	}{
		{tenantHold, "any", "any", true},
		{bucketHold, "b", "k", true},
		{bucketHold, "other", "k", false},
		{objectHold, "b", "k", true},
		{objectHold, "b", "other", false},
	}
	for i, c := range cases {
		if got := c.hold.Matches("T", c.bucket, c.key); got != c.want {
			t.Errorf("case %d: Matches=%v, want %v", i, got, c.want)
		}
	}
	if tenantHold.Matches("Other", "b", "k") {
		t.Error("cross-tenant match must be false")
	}
}

func TestLegalHold_ActiveHonorsExpiryAndRelease(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := LegalHold{TenantID: "T", IssuedBy: "ops", Reason: "case", CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	if !h.Active(now) {
		t.Error("hold should be active at issuance")
	}
	if h.Active(now.Add(48 * time.Hour)) {
		t.Error("hold should expire at ExpiresAt")
	}
	h.Released = true
	if h.Active(now) {
		t.Error("released hold must not be active")
	}
}

func TestMemoryLegalHoldStore_CreateReleaseList(t *testing.T) {
	s := NewMemoryLegalHoldStore()
	ctx := context.Background()
	hold := LegalHold{ID: "h1", TenantID: "T", Reason: "r", IssuedBy: "ops"}
	if err := s.Create(ctx, hold); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(ctx, hold); err == nil {
		t.Error("duplicate id must error")
	}
	got, err := s.List(ctx, "T")
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, err=%v", got, err)
	}
	if err := s.Release(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.Active(ctx, "T", "any", "any")
	if len(got2) != 0 {
		t.Errorf("released hold must be inactive, got %v", got2)
	}
}

func TestCheckDelete_ReturnsErrLegalHoldActive(t *testing.T) {
	s := NewMemoryLegalHoldStore()
	ctx := context.Background()
	_ = s.Create(ctx, LegalHold{ID: "h1", TenantID: "T", Bucket: "b", ObjectKey: "k", Reason: "r", IssuedBy: "ops"})
	if err := CheckDelete(ctx, s, "T", "b", "k"); !errors.Is(err, ErrLegalHoldActive) {
		t.Errorf("CheckDelete = %v, want ErrLegalHoldActive", err)
	}
	if err := CheckDelete(ctx, s, "T", "b", "other"); err != nil {
		t.Errorf("non-matching key must not block: %v", err)
	}
	if err := CheckDelete(ctx, nil, "T", "b", "k"); err != nil {
		t.Errorf("nil store must be a no-op: %v", err)
	}
}
