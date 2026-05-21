// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"log"
	"net"
	"testing"
	"time"
)

// waitDial polls the given TCP address until it accepts a
// connection or the timeout elapses. Used by listener tests that
// spawn their server in a goroutine and need to wait for the
// listener to be ready before issuing requests.
func waitDial(t *testing.T, addr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return lastErr
}

// captureLog redirects the default log.Logger output for the
// duration of fn, returning whatever fn wrote. log.SetOutput is
// the standard hook so this works against the package-level
// log.Printf calls in warnProductionLocalCMK.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	// Drop timestamps so test assertions only depend on the
	// log message itself.
	log.SetFlags(0)
	defer func() {
		log.SetOutput(prev)
		log.SetFlags(prevFlags)
	}()
	fn()
	return buf.String()
}
