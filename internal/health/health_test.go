package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestMonitor_StateReadyWithSoloCell(t *testing.T) {
	m, err := New(Config{NodeID: "gw-1", PollInterval: 10 * time.Millisecond, PollTimeout: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = m.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool { return m.State() == StateReady }, time.Second)
	cancel()
	<-done
}

func TestMonitor_QuorumLostWhenPeerDown(t *testing.T) {
	// Spin up one healthy peer; configure the monitor with two
	// peers so the second is a missing endpoint → quorum needs
	// 2/3 votes (local + one healthy peer = 2) — still met. To
	// force quorum loss we need threshold=3 with only 2 healthy.
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer peer.Close()

	m, err := New(Config{
		NodeID: "gw-local",
		Peers: []Peer{
			{NodeID: "gw-alive", Endpoint: peer.URL},
			{NodeID: "gw-down", Endpoint: "http://127.0.0.1:1"}, // unreachable
		},
		QuorumThreshold: 3,
		PollInterval:    10 * time.Millisecond,
		PollTimeout:     20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = m.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool { return m.State() == StateQuorumLost }, time.Second)
	cancel()
	<-done
}

func TestMonitor_DrainFlipsReadyEndpoint(t *testing.T) {
	m, err := New(Config{NodeID: "gw-1", DrainTimeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Force into ready state by running one poll.
	m.pollOnce(context.Background())
	if m.State() != StateReady {
		t.Fatalf("pre-drain state = %v, want ready", m.State())
	}

	mux := m.ServeMux("")
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// ready should 200.
	resp, err := http.Get(srv.URL + "/internal/ready")
	if err != nil {
		t.Fatalf("GET ready: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ready pre-drain = %d, want 200", resp.StatusCode)
	}

	// POST drain.
	drainResp, err := http.Post(srv.URL+"/internal/drain", "", nil)
	if err != nil {
		t.Fatalf("POST drain: %v", err)
	}
	drainResp.Body.Close()
	if drainResp.StatusCode != http.StatusOK {
		t.Fatalf("drain status = %d, want 200", drainResp.StatusCode)
	}

	// ready should now 503.
	resp2, err := http.Get(srv.URL + "/internal/ready")
	if err != nil {
		t.Fatalf("GET ready post-drain: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("ready post-drain = %d, want 503", resp2.StatusCode)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp2.Body).Decode(&snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.State != StateDraining {
		t.Fatalf("snapshot state = %v, want draining", snap.State)
	}
}

func TestMonitor_TrackBlocksDrain(t *testing.T) {
	m, err := New(Config{NodeID: "gw-1", DrainTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	release := m.Track()
	drainErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		drainErr <- m.Drain(ctx)
	}()
	// Drain should be blocked on inflight > 0.
	time.Sleep(30 * time.Millisecond)
	if m.State() != StateDraining {
		t.Fatalf("drain did not flip state: %v", m.State())
	}
	// Release the request; drain should complete cleanly.
	release()
	if err := <-drainErr; err != nil {
		t.Fatalf("Drain returned error after release: %v", err)
	}
}

func TestMonitor_DrainTimesOutOnStuckInflight(t *testing.T) {
	m, err := New(Config{NodeID: "gw-1", DrainTimeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = m.Track() // never released

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = m.Drain(ctx)
	if err == nil {
		t.Fatalf("Drain with stuck inflight: want error, got nil")
	}
}

func TestQuorumMajority(t *testing.T) {
	cases := map[int]int{0: 1, 1: 1, 2: 2, 3: 2, 4: 3, 5: 3, 6: 4}
	for n, want := range cases {
		if got := QuorumMajority(n); got != want {
			t.Errorf("QuorumMajority(%d) = %d, want %d", n, got, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", within)
}
