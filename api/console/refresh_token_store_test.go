package console

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

// fakeClock is a manually advanced clock so refresh-token expiry can be
// exercised deterministically without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// refreshStoreFactory builds a fresh, empty store bound to cfg.
type refreshStoreFactory func(t *testing.T, cfg RefreshConfig) RefreshTokenStore

func memoryRefreshFactory(t *testing.T, cfg RefreshConfig) RefreshTokenStore {
	t.Helper()
	return NewMemoryRefreshTokenStore(cfg)
}

func sqliteRefreshFactory(t *testing.T, cfg RefreshConfig) RefreshTokenStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "refresh.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteRefreshTokenStore(db, cfg)
	if err != nil {
		t.Fatalf("new sqlite refresh store: %v", err)
	}
	return s
}

// TestRefreshTokenStoreContract runs the same behavioural suite against
// every RefreshTokenStore implementation so the Memory and SQLite
// backends are guaranteed to agree. The Postgres backend runs the same
// suite in postgres_refresh_store_test.go when a DSN is configured.
func TestRefreshTokenStoreContract(t *testing.T) {
	t.Parallel()
	backends := []struct {
		name string
		make refreshStoreFactory
	}{
		{"memory", memoryRefreshFactory},
		{"sqlite", sqliteRefreshFactory},
	}
	for _, b := range backends {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			runRefreshTokenStoreContract(t, b.make)
		})
	}
}

func runRefreshTokenStoreContract(t *testing.T, newStore refreshStoreFactory) {
	t.Run("IssueRequiresTenant", func(t *testing.T) {
		s := newStore(t, RefreshConfig{})
		if _, err := s.Issue(""); err == nil {
			t.Fatal("Issue(\"\"): want error")
		}
	})

	t.Run("IssueThenRotate", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		s := newStore(t, RefreshConfig{TTL: time.Hour, Now: clk.now})
		issued, err := s.Issue("t-1")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if issued.Raw == "" || issued.TenantID != "t-1" {
			t.Fatalf("Issue = %+v; want non-empty Raw and tenant t-1", issued)
		}
		if !issued.ExpiresAt.Equal(clk.now().Add(time.Hour)) {
			t.Fatalf("Issue ExpiresAt = %v; want now+1h", issued.ExpiresAt)
		}
		rotated, err := s.Rotate(issued.Raw)
		if err != nil {
			t.Fatalf("Rotate: %v", err)
		}
		if rotated.Raw == "" || rotated.Raw == issued.Raw {
			t.Fatalf("Rotate Raw = %q; want a fresh token distinct from %q", rotated.Raw, issued.Raw)
		}
		if rotated.TenantID != "t-1" {
			t.Fatalf("Rotate TenantID = %q; want t-1", rotated.TenantID)
		}
	})

	t.Run("UnknownTokenInvalid", func(t *testing.T) {
		s := newStore(t, RefreshConfig{TTL: time.Hour})
		if _, err := s.Rotate("not-a-real-token"); !errors.Is(err, errRefreshTokenInvalid) {
			t.Fatalf("Rotate(unknown) err = %v; want errRefreshTokenInvalid", err)
		}
		if _, err := s.Rotate(""); !errors.Is(err, errRefreshTokenInvalid) {
			t.Fatalf("Rotate(\"\") err = %v; want errRefreshTokenInvalid", err)
		}
	})

	t.Run("RotationChain", func(t *testing.T) {
		s := newStore(t, RefreshConfig{TTL: time.Hour})
		cur, err := s.Issue("t-chain")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		for i := 0; i < 5; i++ {
			next, err := s.Rotate(cur.Raw)
			if err != nil {
				t.Fatalf("Rotate #%d: %v", i, err)
			}
			cur = next
		}
	})

	t.Run("ReuseRevokesFamily", func(t *testing.T) {
		s := newStore(t, RefreshConfig{TTL: time.Hour})
		first, err := s.Issue("t-reuse")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		second, err := s.Rotate(first.Raw)
		if err != nil {
			t.Fatalf("Rotate(first): %v", err)
		}
		// Replaying the already-rotated token is reuse: the store must
		// flag it and revoke the whole family.
		if _, err := s.Rotate(first.Raw); !errors.Is(err, errRefreshTokenReuse) {
			t.Fatalf("Rotate(reused) err = %v; want errRefreshTokenReuse", err)
		}
		// The legitimate successor is now dead too — the family was
		// revoked, so an attacker who stole `second` cannot use it.
		if _, err := s.Rotate(second.Raw); !errors.Is(err, errRefreshTokenInvalid) {
			t.Fatalf("Rotate(successor after family revoke) err = %v; want errRefreshTokenInvalid", err)
		}
	})

	t.Run("Expiry", func(t *testing.T) {
		clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
		s := newStore(t, RefreshConfig{TTL: time.Minute, Now: clk.now})
		issued, err := s.Issue("t-exp")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		clk.advance(2 * time.Minute)
		if _, err := s.Rotate(issued.Raw); !errors.Is(err, errRefreshTokenInvalid) {
			t.Fatalf("Rotate(expired) err = %v; want errRefreshTokenInvalid", err)
		}
	})

	t.Run("Revoke", func(t *testing.T) {
		s := newStore(t, RefreshConfig{TTL: time.Hour})
		issued, err := s.Issue("t-revoke")
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		if err := s.Revoke(issued.Raw); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if _, err := s.Rotate(issued.Raw); !errors.Is(err, errRefreshTokenInvalid) {
			t.Fatalf("Rotate(revoked) err = %v; want errRefreshTokenInvalid", err)
		}
		// Revoking again (or an unknown token) is a no-op, not an error.
		if err := s.Revoke(issued.Raw); err != nil {
			t.Fatalf("Revoke(already revoked): %v", err)
		}
		if err := s.Revoke(""); err != nil {
			t.Fatalf("Revoke(\"\"): %v", err)
		}
	})

	t.Run("RevokeAllForTenant", func(t *testing.T) {
		s := newStore(t, RefreshConfig{TTL: time.Hour})
		a, err := s.Issue("t-multi")
		if err != nil {
			t.Fatalf("Issue a: %v", err)
		}
		b, err := s.Issue("t-multi")
		if err != nil {
			t.Fatalf("Issue b: %v", err)
		}
		other, err := s.Issue("t-other")
		if err != nil {
			t.Fatalf("Issue other: %v", err)
		}
		if err := s.RevokeAllForTenant("t-multi"); err != nil {
			t.Fatalf("RevokeAllForTenant: %v", err)
		}
		for _, dead := range []RefreshToken{a, b} {
			if _, err := s.Rotate(dead.Raw); !errors.Is(err, errRefreshTokenInvalid) {
				t.Fatalf("Rotate(revoked-tenant token) err = %v; want errRefreshTokenInvalid", err)
			}
		}
		// A different tenant's token is untouched.
		if _, err := s.Rotate(other.Raw); err != nil {
			t.Fatalf("Rotate(other tenant) err = %v; want success", err)
		}
		if err := s.RevokeAllForTenant(""); err != nil {
			t.Fatalf("RevokeAllForTenant(\"\"): %v", err)
		}
	})
}

func TestHashRefreshTokenStable(t *testing.T) {
	t.Parallel()
	const raw = "abc123"
	if first, second := hashRefreshToken(raw), hashRefreshToken(raw); first != second {
		t.Fatalf("hashRefreshToken not deterministic: %q vs %q", first, second)
	}
	if hashRefreshToken("a") == hashRefreshToken("b") {
		t.Fatal("hashRefreshToken collision on distinct inputs")
	}
	// The raw token must never equal its stored digest.
	if hashRefreshToken(raw) == raw {
		t.Fatal("hashRefreshToken returned the raw token")
	}
}
