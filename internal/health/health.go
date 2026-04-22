// Package health implements the gateway fleet node health monitor.
//
// A deployment is a cell of N gateway nodes fronted by an external
// load balancer (Linode NodeBalancer or equivalent). This package
// gives each gateway:
//
//   - A liveness endpoint (GET /internal/health) that reports
//     whether the process is alive and able to serve traffic.
//   - A readiness endpoint (GET /internal/ready) that reports
//     whether the node has cell-quorum visibility and has not been
//     drained. Load balancers consult this to decide routing.
//   - A drain endpoint (POST /internal/drain) that flips the node
//     to NotReady so the LB drains in-flight, then optionally
//     empties the local cache tier so the replacement node starts
//     cold rather than stealing peer cache.
//
// The Monitor runs as a background goroutine alongside the
// rebalancer and promotion worker (see cmd/gateway/main.go). A
// context cancellation from SIGTERM drains the node in the same
// shutdown path as the other workers.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
)

// State names the observable node state the monitor exposes.
type State string

const (
	// StateStarting is the state between process start and the
	// first successful peer poll.
	StateStarting State = "starting"

	// StateReady means the local process is healthy and the cell
	// has quorum.
	StateReady State = "ready"

	// StateQuorumLost means the local process is healthy but the
	// cell does not have quorum. The LB may still route traffic
	// but the monitor logs a warning so operators can investigate.
	StateQuorumLost State = "quorum_lost"

	// StateDraining means the operator has asked the node to
	// leave the cell. /ready returns 503; the LB drains in-flight
	// requests and stops sending new ones.
	StateDraining State = "draining"
)

// Peer identifies another gateway in the same cell.
type Peer struct {
	// NodeID is the peer's gateway identifier (used in log lines
	// and cache-tier drain target selection).
	NodeID string
	// Endpoint is the base URL of the peer's internal port, e.g.
	// "http://gw-2.cell-abc.internal:29090".
	Endpoint string
}

// Config captures the monitor's tuning knobs.
type Config struct {
	// NodeID identifies the local gateway in peer /health
	// responses. Required.
	NodeID string

	// CellID identifies the cell. Used only for log context, but
	// recommended so operators can grep.
	CellID string

	// Peers is the cell's peer list (other gateways). The monitor
	// treats the local node as an implicit (N+1)th quorum voter
	// that always votes "yes" so a single-node cell is always in
	// quorum.
	Peers []Peer

	// QuorumThreshold is the minimum number of peers (including
	// the local node) that must report healthy for the cell to
	// be in quorum. Defaults to ceil((N+1)/2).
	QuorumThreshold int

	// PollInterval is the peer poll cadence. Defaults to 2s.
	PollInterval time.Duration

	// PollTimeout bounds one peer GET. Defaults to 1s.
	PollTimeout time.Duration

	// DrainTimeout bounds the total drain wait. Defaults to 30s.
	DrainTimeout time.Duration

	// Cache, when non-nil, is drained on Drain() by evicting every
	// cached piece. Cold-starting the replacement node prevents
	// cache-tier thrash when a stale cache is re-attached to a
	// different data plane.
	Cache hot_object_cache.HotObjectCache

	// HTTPClient, if set, replaces the default *http.Client.
	HTTPClient *http.Client

	// Logger receives state transitions.
	Logger *log.Logger

	// Clock, if set, returns the current time. Tests override it.
	Clock func() time.Time
}

// Monitor is the per-gateway health monitor. A single instance
// exposes the HTTP handlers, drives the peer poll loop, and owns
// the current State. It is safe for concurrent use.
type Monitor struct {
	cfg Config

	mu          sync.RWMutex
	state       State
	lastPoll    time.Time
	peerHealthy map[string]bool
	drainedAt   time.Time
	drainStart  time.Time
	startedAt   time.Time

	// atomic counter: number of in-flight requests gated through
	// Track(). Drain refuses to complete until this reaches zero
	// or the drain deadline fires.
	inflight int64
}

// New builds a Monitor.
func New(cfg Config) (*Monitor, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("health: node_id is required")
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = time.Second
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 30 * time.Second
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.PollTimeout}
	}
	if cfg.QuorumThreshold <= 0 {
		n := len(cfg.Peers) + 1 // + local node
		cfg.QuorumThreshold = (n / 2) + 1
	}
	m := &Monitor{
		cfg:         cfg,
		state:       StateStarting,
		peerHealthy: make(map[string]bool, len(cfg.Peers)),
		startedAt:   cfg.Clock(),
	}
	return m, nil
}

// Run drives the peer poll loop until ctx is cancelled. On
// cancellation Run returns nil after triggering a best-effort
// drain so the gateway can share a single shutdown context with
// the other background workers.
func (m *Monitor) Run(ctx context.Context) error {
	t := time.NewTicker(m.cfg.PollInterval)
	defer t.Stop()
	// Kick off a first poll immediately so /ready transitions out
	// of StateStarting without waiting for the first tick.
	m.pollOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			drainCtx, cancel := context.WithTimeout(context.Background(), m.cfg.DrainTimeout)
			_ = m.Drain(drainCtx)
			cancel()
			return nil
		case <-t.C:
			m.pollOnce(ctx)
		}
	}
}

// pollOnce sweeps every peer, records its health, and updates the
// observable state.
func (m *Monitor) pollOnce(ctx context.Context) {
	healthy := make(map[string]bool, len(m.cfg.Peers))
	for _, p := range m.cfg.Peers {
		healthy[p.NodeID] = m.probePeer(ctx, p)
	}
	m.mu.Lock()
	m.peerHealthy = healthy
	m.lastPoll = m.cfg.Clock()
	if m.state == StateDraining {
		m.mu.Unlock()
		return
	}
	// Count local node as healthy for quorum math.
	healthyCount := 1
	for _, ok := range healthy {
		if ok {
			healthyCount++
		}
	}
	prev := m.state
	if healthyCount >= m.cfg.QuorumThreshold {
		m.state = StateReady
	} else {
		m.state = StateQuorumLost
	}
	m.mu.Unlock()
	if prev != m.state {
		m.logf("health: state transition %s -> %s (healthy=%d threshold=%d cell=%s)",
			prev, m.state, healthyCount, m.cfg.QuorumThreshold, m.cfg.CellID)
	}
}

// probePeer issues a bounded GET /internal/health against peer p.
// Network or 5xx failures report the peer unhealthy; 2xx / 3xx
// report it healthy. 4xx is treated as "peer is up but rejecting
// us", which still counts as alive for quorum — the LB handles
// authn separately.
func (m *Monitor) probePeer(ctx context.Context, p Peer) bool {
	probeCtx, cancel := context.WithTimeout(ctx, m.cfg.PollTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, strings.TrimRight(p.Endpoint, "/")+"/internal/health", nil)
	if err != nil {
		return false
	}
	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}

// Snapshot is the observable monitor state.
type Snapshot struct {
	NodeID          string          `json:"node_id"`
	CellID          string          `json:"cell_id,omitempty"`
	State           State           `json:"state"`
	LastPoll        time.Time       `json:"last_poll,omitempty"`
	StartedAt       time.Time       `json:"started_at"`
	QuorumThreshold int             `json:"quorum_threshold"`
	HealthyPeers    map[string]bool `json:"healthy_peers,omitempty"`
	Inflight        int64           `json:"inflight"`
	DrainStart      time.Time       `json:"drain_start,omitempty"`
	DrainedAt       time.Time       `json:"drained_at,omitempty"`
}

// State returns the current observable state.
func (m *Monitor) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// Snapshot returns a deep-copied view of the monitor's current
// state. Safe to expose over HTTP.
func (m *Monitor) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	peers := make(map[string]bool, len(m.peerHealthy))
	for k, v := range m.peerHealthy {
		peers[k] = v
	}
	return Snapshot{
		NodeID:          m.cfg.NodeID,
		CellID:          m.cfg.CellID,
		State:           m.state,
		LastPoll:        m.lastPoll,
		StartedAt:       m.startedAt,
		QuorumThreshold: m.cfg.QuorumThreshold,
		HealthyPeers:    peers,
		Inflight:        atomic.LoadInt64(&m.inflight),
		DrainStart:      m.drainStart,
		DrainedAt:       m.drainedAt,
	}
}

// Track should be used by request handlers that want the drain
// path to wait for their in-flight work. It returns a release
// function the caller must invoke when the request finishes.
func (m *Monitor) Track() func() {
	atomic.AddInt64(&m.inflight, 1)
	return func() { atomic.AddInt64(&m.inflight, -1) }
}

// Drain flips the monitor to StateDraining, waits for in-flight
// requests to finish (bounded by ctx), and empties the cache tier
// if one is configured. It is safe to call multiple times — later
// calls return immediately once the first drain has settled.
func (m *Monitor) Drain(ctx context.Context) error {
	m.mu.Lock()
	if m.state == StateDraining {
		m.mu.Unlock()
		return m.waitInflight(ctx)
	}
	m.state = StateDraining
	m.drainStart = m.cfg.Clock()
	m.mu.Unlock()
	m.logf("health: node %s entering drain (cell=%s)", m.cfg.NodeID, m.cfg.CellID)
	if err := m.waitInflight(ctx); err != nil {
		return err
	}
	if m.cfg.Cache != nil {
		if err := m.drainCache(ctx); err != nil {
			m.logf("health: cache drain returned: %v", err)
		}
	}
	m.mu.Lock()
	m.drainedAt = m.cfg.Clock()
	m.mu.Unlock()
	m.logf("health: node %s drain complete (cell=%s)", m.cfg.NodeID, m.cfg.CellID)
	return nil
}

func (m *Monitor) waitInflight(ctx context.Context) error {
	for {
		if atomic.LoadInt64(&m.inflight) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// drainCache evicts every entry from the local cache. Phase 3
// deliberately cold-starts the replacement node rather than
// streaming the local cache to a peer — cross-gateway cache
// warm-up is tracked as a Phase 4 follow-up.
func (m *Monitor) drainCache(ctx context.Context) error {
	stats := m.cfg.Cache.Stats()
	// Peer list gives operators a way to report the drain but the
	// iteration itself goes through the local cache.
	_ = stats
	// The HotObjectCache interface doesn't expose "list all keys",
	// so we rely on the promotion worker being idle during drain
	// and let the cache naturally age out. For caches that expose
	// keyable iteration (DiskCache / MemoryCache), operators can
	// cast and call Evict per piece; the monitor keeps the
	// interface stable.
	if lister, ok := m.cfg.Cache.(cacheLister); ok {
		for _, id := range lister.List() {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := m.cfg.Cache.Evict(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// cacheLister is an optional extension implemented by caches that
// can enumerate their keys. DiskCache implements this by scanning
// the shard directory; MemoryCache by walking its index.
type cacheLister interface {
	List() []string
}

func (m *Monitor) logf(format string, args ...any) {
	if m.cfg.Logger == nil {
		return
	}
	m.cfg.Logger.Printf(format, args...)
}

// ServeMux returns an http.Handler with the monitor's three
// endpoints mounted under the given prefix. Deployments mount
// this on an internal port (distinct from the public S3 port) so
// the LB health probes and operator tooling can reach it without
// going through the S3 authenticator.
func (m *Monitor) ServeMux(prefix string) http.Handler {
	prefix = strings.TrimRight(prefix, "/")
	mux := http.NewServeMux()
	mux.HandleFunc(prefix+"/internal/health", m.handleHealth)
	mux.HandleFunc(prefix+"/internal/ready", m.handleReady)
	mux.HandleFunc(prefix+"/internal/drain", m.handleDrain)
	return mux
}

func (m *Monitor) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, m.Snapshot())
}

func (m *Monitor) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s := m.Snapshot()
	code := http.StatusOK
	if s.State != StateReady {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, s)
}

func (m *Monitor) handleDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), m.cfg.DrainTimeout)
	defer cancel()
	if err := m.Drain(ctx); err != nil {
		writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, m.Snapshot())
}

// SortedPeerIDs returns the monitor's peer list sorted by NodeID.
// Useful for deterministic logging / dashboards.
func (m *Monitor) SortedPeerIDs() []string {
	ids := make([]string, 0, len(m.cfg.Peers))
	for _, p := range m.cfg.Peers {
		ids = append(ids, p.NodeID)
	}
	sort.Strings(ids)
	return ids
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// QuorumMajority returns the smallest integer greater than half
// of n. Exported so operators can pre-compute the threshold from
// the cell size when building Config.
func QuorumMajority(n int) int {
	if n <= 0 {
		return 1
	}
	return (n / 2) + 1
}
