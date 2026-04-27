package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

type errProvider struct{}

func (errProvider) Name() string { return "err" }
func (errProvider) Block(context.Context, string, time.Duration, string) error {
	return errors.New("nope")
}
func (errProvider) Unblock(context.Context, string) error { return errors.New("nope") }

func TestCompositeShield_FansOutAndJoinsErrors(t *testing.T) {
	mem := &MemoryShield{}
	c := &CompositeShield{Providers: []DDoSProvider{mem, errProvider{}}}
	err := c.Block(context.Background(), "10.0.0.1", time.Minute, "test")
	if err == nil {
		t.Fatal("expected joined error from failing provider")
	}
	if !mem.IsBlocked("10.0.0.1") {
		t.Error("memory provider must still see Block")
	}
}

func TestMemoryShield_BlockExpires(t *testing.T) {
	m := &MemoryShield{}
	if err := m.Block(context.Background(), "1.2.3.4", -time.Second, "expired"); err != nil {
		t.Fatal(err)
	}
	if m.IsBlocked("1.2.3.4") {
		t.Error("expired block must report not blocked")
	}
}

func TestMemoryShield_RecordsCalls(t *testing.T) {
	m := &MemoryShield{}
	_ = m.Block(context.Background(), "1.1.1.1", time.Minute, "abuse")
	_ = m.Unblock(context.Background(), "1.1.1.1")
	calls := m.Calls()
	if len(calls) != 2 {
		t.Fatalf("len(calls)=%d, want 2", len(calls))
	}
	if calls[0].Op != "block" || calls[1].Op != "unblock" {
		t.Errorf("ops=%q,%q", calls[0].Op, calls[1].Op)
	}
}

func TestCloudflareProvider_RejectsMissingCreds(t *testing.T) {
	p := &CloudflareProvider{}
	if err := p.Block(context.Background(), "1.1.1.1", time.Minute, "test"); err == nil {
		t.Error("missing creds must error")
	}
	if err := p.Unblock(context.Background(), "1.1.1.1"); err == nil {
		t.Error("missing creds must error on unblock")
	}
}

func TestClassifyTarget(t *testing.T) {
	if classifyTarget("1.1.1.1") != "ip" {
		t.Error("IPv4 must classify as ip")
	}
	if classifyTarget("2001:db8::1") != "ip6" {
		t.Error("IPv6 must classify as ip6")
	}
}
