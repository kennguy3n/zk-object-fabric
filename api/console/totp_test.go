package console

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// rfc6238Secret is the ASCII seed "12345678901234567890" the RFC 6238
// Appendix B test vectors use, base32-encoded the way an authenticator
// app stores it.
const rfc6238Secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"

// TestHOTPCodeMatchesRFC6238Vectors pins the HOTP core against the
// official RFC 6238 Appendix B SHA-1 vectors. The vectors publish
// 8-digit codes; this implementation emits totpDigits (6), so we compare
// the low totpDigits digits — i.e. the same dynamic-truncation output
// reduced to our width. A regression in the HMAC, big-endian counter, or
// truncation math would break these.
func TestHOTPCodeMatchesRFC6238Vectors(t *testing.T) {
	key, err := decodeTOTPSecret(rfc6238Secret)
	if err != nil {
		t.Fatalf("decode rfc secret: %v", err)
	}
	cases := []struct {
		unix    int64
		eightDg string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	}
	for _, c := range cases {
		step := totpStep(time.Unix(c.unix, 0).UTC())
		got := hotpCode(key, step)
		want := c.eightDg[len(c.eightDg)-totpDigits:]
		if got != want {
			t.Errorf("hotpCode at unix %d = %s, want %s (low %d of %s)",
				c.unix, got, want, totpDigits, c.eightDg)
		}
	}
}

// TestTOTPVerifyAcceptsCurrentCode confirms a code generated for the
// current step verifies, and reports that step back for replay tracking.
func TestTOTPVerifyAcceptsCurrentCode(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	key, _ := decodeTOTPSecret(rfc6238Secret)
	code := hotpCode(key, totpStep(now))

	step, ok := totpVerify(rfc6238Secret, code, now)
	if !ok {
		t.Fatal("current code did not verify")
	}
	if step != totpStep(now) {
		t.Fatalf("matched step = %d, want %d", step, totpStep(now))
	}
}

// TestTOTPVerifyAllowsOneStepSkew confirms the ±1 step window: a code
// from the immediately previous and next step still verifies (clock
// drift + human typing latency), but two steps away does not.
func TestTOTPVerifyAllowsOneStepSkew(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	key, _ := decodeTOTPSecret(rfc6238Secret)
	period := int64(totpPeriod / time.Second)

	prev := hotpCode(key, totpStep(now)-1)
	next := hotpCode(key, totpStep(now)+1)
	if _, ok := totpVerify(rfc6238Secret, prev, now); !ok {
		t.Error("previous-step code should verify within skew window")
	}
	if _, ok := totpVerify(rfc6238Secret, next, now); !ok {
		t.Error("next-step code should verify within skew window")
	}

	// Two steps in the past is outside the window.
	twoAgo := hotpCode(key, totpStep(now)-2)
	if _, ok := totpVerify(rfc6238Secret, twoAgo, now); ok {
		t.Error("code two steps old should be rejected")
	}
	// Sanity: that same code does verify if the clock is rewound by
	// two periods, proving it's the window — not the code — rejecting.
	if _, ok := totpVerify(rfc6238Secret, twoAgo, now.Add(-2*time.Duration(period)*time.Second)); !ok {
		t.Error("two-steps-old code should verify against the matching past instant")
	}
}

// TestTOTPVerifyRejectsMalformed covers the up-front shape checks:
// wrong length, non-digit characters, empty input, and a wrong-but-
// well-formed code all fail without panicking.
func TestTOTPVerifyRejectsMalformed(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	for _, bad := range []string{"", "1234", "1234567", "abcdef", "12 456", "000000"} {
		if _, ok := totpVerify(rfc6238Secret, bad, now); ok {
			// "000000" is astronomically unlikely to be the live
			// code; treat a match as a real failure signal.
			t.Errorf("malformed/wrong code %q unexpectedly verified", bad)
		}
	}
}

// TestNewTOTPSecretDecodes confirms a freshly minted secret is valid
// base32 of the expected width and that two calls differ.
func TestNewTOTPSecretDecodes(t *testing.T) {
	a, err := newTOTPSecret()
	if err != nil {
		t.Fatalf("newTOTPSecret: %v", err)
	}
	raw, err := decodeTOTPSecret(a)
	if err != nil {
		t.Fatalf("decode fresh secret: %v", err)
	}
	if len(raw) != totpSecretBytes {
		t.Fatalf("secret entropy = %d bytes, want %d", len(raw), totpSecretBytes)
	}
	b, _ := newTOTPSecret()
	if a == b {
		t.Fatal("two generated secrets collided")
	}
}

// TestTOTPEnrollmentURI checks the otpauth URI is well-formed and
// carries the secret, issuer, and algorithm parameters an authenticator
// app needs.
func TestTOTPEnrollmentURI(t *testing.T) {
	uri := totpEnrollmentURI("zk-object-fabric", "user@example.com", rfc6238Secret)
	if !strings.HasPrefix(uri, "otpauth://totp/") {
		t.Fatalf("uri missing otpauth scheme: %s", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse uri: %v", err)
	}
	q := u.Query()
	if q.Get("secret") != rfc6238Secret {
		t.Errorf("uri secret = %q, want %q", q.Get("secret"), rfc6238Secret)
	}
	if q.Get("issuer") != "zk-object-fabric" {
		t.Errorf("uri issuer = %q, want zk-object-fabric", q.Get("issuer"))
	}
	if q.Get("digits") != "6" {
		t.Errorf("uri digits = %q, want 6", q.Get("digits"))
	}
}
