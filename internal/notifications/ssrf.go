package notifications

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// newGuardedClient builds the *http.Client the dispatcher uses to POST
// webhooks when the caller did not inject one. Unless allowPrivate is
// set, the client refuses to connect to non-public addresses (loopback,
// link-local — including the 169.254.169.254 cloud metadata endpoint —
// and private RFC 1918 / ULA ranges).
//
// The check runs in the dialer's Control hook, which fires after DNS
// resolution with the concrete IP about to be connected, so it also
// defeats DNS-rebinding: a hostname that resolves to a public address at
// PutBucketNotificationConfiguration time but to 127.0.0.1 at delivery
// time is still blocked.
func newGuardedClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = guardDial
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// guardDial is a net.Dialer.Control hook that rejects connections to
// non-public IP addresses. address is the post-resolution host:port the
// dialer is about to connect to.
func guardDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("notifications: cannot parse dial address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("notifications: cannot parse dial IP %q", host)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("notifications: refusing to deliver to non-public address %s (SSRF guard)", ip)
	}
	return nil
}

// isPublicIP reports whether ip is a globally routable unicast address.
// It rejects unspecified, loopback, link-local (unicast and multicast,
// covering the 169.254.169.254 metadata service), private (RFC 1918 and
// IPv6 ULA fc00::/7), and any multicast address.
func isPublicIP(ip net.IP) bool {
	if ip.IsUnspecified() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return false
	}
	return true
}
