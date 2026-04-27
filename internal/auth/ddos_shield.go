package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

// DDoSShield is the integration point for upstream DDoS
// mitigation services (Cloudflare, Akamai, or any vendor with a
// "block this IP for N seconds" REST API).
//
// The gateway calls Block when it sees evidence of a volumetric
// attack — typically the abuse guard's anomaly stream or the
// rate limiter's burst-window violation list. Implementations
// fan out to one or more upstream provider clients in parallel;
// failures on individual providers are logged but do not block
// the caller.
type DDoSShield interface {
	// Block asks every upstream provider to drop traffic from
	// addr for at least ttl. addr is an IPv4 or IPv6 address
	// without a CIDR prefix; reason is a short operator string
	// surfaced by some providers in their dashboards.
	Block(ctx context.Context, addr string, ttl time.Duration, reason string) error

	// Unblock removes any active rule that Block previously
	// created. Implementations MAY ignore errors when no rule
	// exists.
	Unblock(ctx context.Context, addr string) error
}

// DDoSProvider is a single upstream client. Multiple providers
// can be combined into a CompositeShield.
type DDoSProvider interface {
	Name() string
	Block(ctx context.Context, addr string, ttl time.Duration, reason string) error
	Unblock(ctx context.Context, addr string) error
}

// CompositeShield fans out Block / Unblock to every Provider in
// parallel. A nil or empty CompositeShield is a no-op.
type CompositeShield struct {
	Providers []DDoSProvider
}

// Block fans out to every provider; partial failures are
// returned as a joined error so the caller can surface them
// without preventing successful providers from acting.
func (c *CompositeShield) Block(ctx context.Context, addr string, ttl time.Duration, reason string) error {
	if c == nil || len(c.Providers) == 0 {
		return nil
	}
	return runFanout(ctx, c.Providers, "block", func(p DDoSProvider) error {
		return p.Block(ctx, addr, ttl, reason)
	})
}

// Unblock fans out to every provider in parallel.
func (c *CompositeShield) Unblock(ctx context.Context, addr string) error {
	if c == nil || len(c.Providers) == 0 {
		return nil
	}
	return runFanout(ctx, c.Providers, "unblock", func(p DDoSProvider) error {
		return p.Unblock(ctx, addr)
	})
}

func runFanout(ctx context.Context, providers []DDoSProvider, op string, fn func(DDoSProvider) error) error {
	var wg sync.WaitGroup
	errs := make([]string, 0, len(providers))
	var mu sync.Mutex
	for _, p := range providers {
		wg.Add(1)
		go func(p DDoSProvider) {
			defer wg.Done()
			if err := fn(p); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Sprintf("%s/%s: %v", p.Name(), op, err))
				mu.Unlock()
			}
		}(p)
	}
	wg.Wait()
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// CloudflareProvider is a thin wrapper around Cloudflare's
// "firewall access rules" REST endpoint. It is intentionally
// minimal — production deployments are expected to swap it for
// the canonical SDK once the gateway depends on cloudflare-go.
type CloudflareProvider struct {
	APIToken  string
	AccountID string
	HTTP      *http.Client
}

// Name implements DDoSProvider.
func (CloudflareProvider) Name() string { return "cloudflare" }

// Block creates a "block" firewall access rule for addr.
func (c *CloudflareProvider) Block(ctx context.Context, addr string, ttl time.Duration, reason string) error {
	if c.AccountID == "" || c.APIToken == "" {
		return errors.New("cloudflare: account_id or api_token missing")
	}
	body := map[string]any{
		"mode": "block",
		"configuration": map[string]string{
			"target": classifyTarget(addr),
			"value":  addr,
		},
		"notes": reason,
	}
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/firewall/access_rules/rules", c.AccountID)
	return c.do(ctx, http.MethodPost, url, body)
}

// Unblock deletes the most-recent "block" rule that targets
// addr. The Cloudflare API does not expose delete-by-target
// directly, so the production implementation should query first
// and then delete by rule ID; the placeholder DELETE here exists
// to satisfy the DDoSProvider interface for tests.
func (c *CloudflareProvider) Unblock(ctx context.Context, addr string) error {
	if c.AccountID == "" || c.APIToken == "" {
		return errors.New("cloudflare: account_id or api_token missing")
	}
	// Escape addr because IPv6 addresses contain ":" which is a
	// reserved character in URL query strings; passing the raw
	// value would build an invalid URL on every IPv6 unblock.
	url := fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/firewall/access_rules/rules?configuration.value=%s",
		c.AccountID, neturl.QueryEscape(addr))
	return c.do(ctx, http.MethodDelete, url, nil)
}

func (c *CloudflareProvider) do(ctx context.Context, method, url string, body any) error {
	cli := c.HTTP
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Second}
	}
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(raw))
	}
	var req *http.Request
	var err error
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, url, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, url, nil)
	}
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("cloudflare: http %d", resp.StatusCode)
	}
	return nil
}

func classifyTarget(addr string) string {
	if strings.Contains(addr, ":") {
		return "ip6"
	}
	return "ip"
}

// MemoryShield is a DDoSShield + DDoSProvider implementation
// useful for tests and dev environments. It records every
// Block / Unblock call so assertions can verify the gateway is
// driving the integration correctly.
type MemoryShield struct {
	mu      sync.Mutex
	blocked map[string]time.Time
	calls   []ShieldCall
}

// ShieldCall captures one operator action against the shield.
type ShieldCall struct {
	Op     string
	Addr   string
	Reason string
	TTL    time.Duration
	At     time.Time
}

// Name implements DDoSProvider.
func (*MemoryShield) Name() string { return "memory" }

// Block records the call.
func (m *MemoryShield) Block(ctx context.Context, addr string, ttl time.Duration, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.blocked == nil {
		m.blocked = map[string]time.Time{}
	}
	m.blocked[addr] = time.Now().Add(ttl)
	m.calls = append(m.calls, ShieldCall{Op: "block", Addr: addr, Reason: reason, TTL: ttl, At: time.Now()})
	return nil
}

// Unblock records the call.
func (m *MemoryShield) Unblock(ctx context.Context, addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blocked, addr)
	m.calls = append(m.calls, ShieldCall{Op: "unblock", Addr: addr, At: time.Now()})
	return nil
}

// Calls returns a copy of the recorded operator actions.
func (m *MemoryShield) Calls() []ShieldCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ShieldCall, len(m.calls))
	copy(out, m.calls)
	return out
}

// IsBlocked returns true if addr is currently in the active
// block list.
func (m *MemoryShield) IsBlocked(addr string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	exp, ok := m.blocked[addr]
	if !ok {
		return false
	}
	return time.Now().Before(exp)
}
