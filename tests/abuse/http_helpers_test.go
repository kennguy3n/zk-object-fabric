package abuse

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// newHTTPClient returns an http.Client that does not follow
// redirects, disables HTTP/2 (the abuse tests need to count
// distinct TCP connections), and applies the supplied total
// deadline to every request.
func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			ForceAttemptHTTP2:   false,
			MaxConnsPerHost:     64,
			IdleConnTimeout:     time.Second,
			DisableCompression:  true,
			DisableKeepAlives:   false,
			TLSHandshakeTimeout: timeout,
		},
	}
}

// newGETRequest is a tiny wrapper around http.NewRequest that
// returns a request usable by the harness's HTTP client.
func newGETRequest(url string) (*http.Request, error) {
	return http.NewRequest("GET", url, nil)
}

// statusError carries a non-200 status code as a Go error so
// tests can use errors.Is / assertion helpers without unwrapping
// the response.
type statusError struct{ code int }

func (e statusError) Error() string { return fmt.Sprintf("http status %d", e.code) }

func errStatus(code int) error { return statusError{code: code} }

// drainBody copies the response body into io.Discard so the
// underlying TCP connection can be returned to the keep-alive
// pool. Tests that don't drain may exhaust the pool and start
// dialling new connections, which obscures connection-counting
// assertions.
func drainBody(body io.Reader) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
}

// containsAny reports whether haystack contains any of needles.
// Used by tests that match against an error message that may
// vary across Go versions (e.g. "connection reset by peer" vs
// "broken pipe").
func containsAny(haystack string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}
