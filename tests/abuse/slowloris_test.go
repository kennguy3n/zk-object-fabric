package abuse

import (
	"bufio"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSlowloris_ReadHeaderTimeoutClosesConnection pins the
// production-side defence introduced in
// internal/config/config.go and wired into the gateway server in
// cmd/gateway/main.go: a Slowloris client that opens a TCP
// connection and dribbles header bytes must be evicted within
// ReadHeaderTimeout regardless of how slowly the bytes arrive.
//
// The test boots the harness with a 200ms ReadHeaderTimeout,
// opens a raw TCP connection, sends the request line + a single
// header byte, then blocks reading. A regression that drops
// ReadHeaderTimeout (or sets it to zero, which Go reads as
// "fall back to ReadTimeout") would let this connection stay
// open until the test times out.
func TestSlowloris_ReadHeaderTimeoutClosesConnection(t *testing.T) {
	t.Parallel()
	h := NewHarness(t, HarnessConfig{
		ReadHeaderTimeout: 200 * time.Millisecond,
		IdleTimeout:       200 * time.Millisecond,
	})

	addr := h.Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Write the request line + a single header line that does
	// not terminate (no trailing CRLF CRLF). A compliant Go
	// http.Server with no ReadHeaderTimeout would block here
	// forever; with ReadHeaderTimeout configured it must close
	// the connection within the configured timeout.
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: example.com\r\nX-Slowloris: 1")); err != nil {
		t.Fatalf("write partial headers: %v", err)
	}

	// Pin a hard deadline 5x the configured ReadHeaderTimeout
	// so a regression that drops the timeout is visible as a
	// test failure rather than a hang.
	deadline := time.Now().Add(5 * 200 * time.Millisecond)
	_ = conn.SetReadDeadline(deadline)

	buf := make([]byte, 1024)
	start := time.Now()
	_, err = conn.Read(buf)
	elapsed := time.Since(start)

	// The server should close the connection (io.EOF) or the
	// read should fail with a "use of closed network connection"
	// once ReadHeaderTimeout fires server-side. Either error is
	// a pass; the failure mode we want to catch is "no error,
	// the connection is still open".
	if err == nil {
		t.Fatalf("connection still open after %s; ReadHeaderTimeout did not fire", elapsed)
	}
	if elapsed > 4*200*time.Millisecond {
		t.Fatalf("connection closed after %s, expected close within ~%s", elapsed, 2*200*time.Millisecond)
	}
	t.Logf("server closed slowloris connection after %s (timeout=%s): %v", elapsed, 200*time.Millisecond, err)
}

// TestSlowloris_ManyConnections_DoNotExhaustGoroutines fans out
// MaxConnections raw TCP connections that each send a partial
// header, then verifies that legitimate requests still succeed
// even while the slow connections are pending. The harness's
// ReadHeaderTimeout should reap every slow connection in O(N)
// time bounded by the timeout, not O(N * ReadTimeout).
func TestSlowloris_ManyConnections_DoNotExhaustGoroutines(t *testing.T) {
	t.Parallel()
	const MaxConnections = 64
	h := NewHarness(t, HarnessConfig{
		Tenants: []TenantSpec{{
			ID: "t-loris", AccessKey: "AKLORIS", SecretKey: "secret-loris",
			RPS: 100, Burst: 100,
		}},
		ReadHeaderTimeout: 250 * time.Millisecond,
		IdleTimeout:       250 * time.Millisecond,
	})

	// Open many slow connections; do not let them complete.
	addr := h.Listener.Addr().String()
	slowConns := make([]net.Conn, 0, MaxConnections)
	defer func() {
		for _, c := range slowConns {
			_ = c.Close()
		}
	}()
	for i := 0; i < MaxConnections; i++ {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatalf("dial slow conn %d: %v", i, err)
		}
		// Send a single byte so the server reads "something"
		// and starts the ReadHeaderTimeout clock.
		if _, err := c.Write([]byte("G")); err != nil {
			t.Fatalf("write to slow conn %d: %v", i, err)
		}
		slowConns = append(slowConns, c)
	}

	// While the slow connections are pending, drive a real
	// presigned request on a separate connection. It should
	// succeed within a few hundred ms (no head-of-line block).
	deadline := time.After(2 * time.Second)
	done := make(chan struct{})
	var resp200 int
	var lastErr error
	go func() {
		defer close(done)
		url := h.PresignedGet("t-loris", "fast-path")
		req, err := newGETRequest(url)
		if err != nil {
			lastErr = err
			return
		}
		client := newHTTPClient(2 * time.Second)
		r, err := client.Do(req)
		if err != nil {
			lastErr = err
			return
		}
		defer r.Body.Close()
		_, _ = io.Copy(io.Discard, r.Body)
		if r.StatusCode == 200 {
			resp200++
		} else {
			lastErr = errStatus(r.StatusCode)
		}
	}()

	select {
	case <-done:
	case <-deadline:
		t.Fatal("legitimate request did not complete within 2s while slowloris connections held; head-of-line blocking?")
	}
	if lastErr != nil {
		t.Fatalf("legitimate request failed under slowloris load: %v", lastErr)
	}
	if resp200 != 1 {
		t.Fatalf("legitimate request got %d 200s, want 1", resp200)
	}

	// Now wait one ReadHeaderTimeout window + slack and confirm
	// every slow connection has actually been reaped. We do not
	// just count goroutines (flaky) — we read from each conn and
	// expect EOF / "connection closed".
	time.Sleep(500 * time.Millisecond)
	var reaped, stillOpen int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, c := range slowConns {
		wg.Add(1)
		go func(i int, c net.Conn) {
			defer wg.Done()
			// Drain bytes until EOF or timeout. A reaped
			// connection follows one of two paths in Go's
			// http.Server: (a) immediate close (Read => EOF
			// on first call), or (b) write a 408 / 400
			// response and then close (Read => N bytes,
			// followed by EOF / closed). Either path is a
			// pass; the failure mode is "Read never EOFs
			// because the server is still waiting for more
			// header bytes".
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			b := make([]byte, 1024)
			closed := false
			for {
				n, err := c.Read(b)
				if err != nil {
					if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "closed") || strings.Contains(err.Error(), "reset") {
						closed = true
					}
					break
				}
				if n == 0 {
					break
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if closed {
				reaped++
			} else {
				stillOpen++
				t.Logf("conn %d never EOFed after timeout window", i)
			}
		}(i, c)
	}
	wg.Wait()

	if stillOpen > 0 {
		t.Fatalf("%d/%d slowloris connections still open after timeout window; ReadHeaderTimeout reap did not fire", stillOpen, MaxConnections)
	}
	t.Logf("reaped %d/%d slowloris connections; legitimate path served 1 request", reaped, MaxConnections)
}

// TestMaxHeaderBytes_RejectsOversizedHeaders pins the
// MaxHeaderBytes knob. A request with a header that exceeds the
// configured ceiling must be rejected at parse time with HTTP
// 431, regardless of how slowly the bytes arrive.
func TestMaxHeaderBytes_RejectsOversizedHeaders(t *testing.T) {
	t.Parallel()
	const Cap = 4096
	h := NewHarness(t, HarnessConfig{
		ReadHeaderTimeout: time.Second,
		MaxHeaderBytes:    Cap,
	})

	addr := h.Listener.Addr().String()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Compose an oversized header. The padding header is 8x
	// the cap to force the parser to abort even with generous
	// per-line allowances.
	padding := strings.Repeat("A", 8*Cap)
	req := "GET / HTTP/1.1\r\nHost: example.com\r\nX-Huge: " + padding + "\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		// On some platforms the server may RST the connection
		// before the full write completes. Treat a write error
		// as evidence the server rejected the request, but
		// still try to read the response.
		t.Logf("write returned %v (server may have closed mid-write)", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		// Connection reset is a valid rejection mode for an
		// oversized request — Go's http.Server may close
		// before flushing a status line.
		t.Logf("read after oversized header: %v (connection reset is a valid rejection)", err)
		return
	}
	// HTTP/1.1 431 Request Header Fields Too Large is the
	// canonical response Go's http.Server sends.
	if !strings.Contains(line, "431") && !strings.Contains(line, "400") {
		t.Fatalf("status line = %q; expected 431 or 400 for oversized header", strings.TrimSpace(line))
	}
}
