package notifications

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip     string
		public bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"203.0.113.10", true},
		{"2606:4700:4700::1111", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"169.254.169.254", false}, // cloud metadata service
		{"169.254.0.1", false},
		{"10.0.0.5", false},
		{"172.16.4.4", false},
		{"192.168.1.1", false},
		{"0.0.0.0", false},
		{"fc00::1", false}, // IPv6 ULA
		{"fe80::1", false}, // IPv6 link-local
		{"224.0.0.1", false},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad test IP %q", c.ip)
		}
		if got := isPublicIP(ip); got != c.public {
			t.Errorf("isPublicIP(%s) = %v, want %v", c.ip, got, c.public)
		}
	}
}

func TestGuardDialBlocksPrivate(t *testing.T) {
	if err := guardDial("tcp", "127.0.0.1:8080", nil); err == nil {
		t.Error("guardDial allowed loopback")
	}
	if err := guardDial("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("guardDial allowed metadata IP")
	}
	if err := guardDial("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("guardDial blocked a public IP: %v", err)
	}
}

// TestGuardedClientRefusesLoopback drives the full default client (no
// injected Doer) against a real loopback httptest server and asserts the
// SSRF guard refuses the connection, so a tenant cannot point a webhook
// at an internal address.
func TestGuardedClientRefusesLoopback(t *testing.T) {
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	dead := &recordingDeadLetters{}
	d, err := New(Config{
		Source:      stubSource{cfg: putRule(srv.URL)}, // srv.URL is http://127.0.0.1:PORT
		Workers:     1,
		MaxAttempts: 1,
		DeadLetters: dead,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	waitFor(t, func() bool { return dead.len() == 1 })
	if hit != 0 {
		t.Errorf("loopback server was reached %d times; SSRF guard should have blocked it", hit)
	}
	dead.mu.Lock()
	dl := dead.dead[0]
	dead.mu.Unlock()
	if dl.Attempts != 1 {
		t.Errorf("dead letter attempts = %d, want 1", dl.Attempts)
	}
}

// TestGuardedClientAllowsPrivateWhenConfigured confirms the opt-out lets
// the dev profile deliver to localhost receivers.
func TestGuardedClientAllowsPrivateWhenConfigured(t *testing.T) {
	var hit int
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		w.WriteHeader(http.StatusOK)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	d, err := New(Config{
		Source:                   stubSource{cfg: putRule(srv.URL)},
		Workers:                  1,
		AllowPrivateDestinations: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	d.Notify(Event{TenantID: "t1", Bucket: "b1", Name: notification.ObjectCreatedPut, ObjectKey: "k"})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("localhost receiver was never reached despite AllowPrivateDestinations")
	}
	waitFor(t, func() bool { return d.Stats().Delivered == 1 })
}
