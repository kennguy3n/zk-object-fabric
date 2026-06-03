package console

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// TOTP (RFC 6238) is implemented in-house rather than pulling in a
// third-party dependency, matching the rest of this codebase's
// hand-rolled crypto primitives (SigV4 signing, per-chunk AAD binding,
// the refresh-token substrate). The algorithm is small, the standard is
// stable, and keeping it in-tree means the security-critical path has no
// unaudited external code. The defaults below are the ones every
// mainstream authenticator app (Google Authenticator, 1Password, Authy)
// assumes when scanning an otpauth:// URI, so an enrolled secret works
// without the user configuring anything.
const (
	// totpPeriod is the time step: a new code every 30 seconds.
	totpPeriod = 30 * time.Second

	// totpDigits is the number of decimal digits in a code. Six is
	// the universal authenticator-app default.
	totpDigits = 6

	// totpSkewSteps is how many time steps on either side of the
	// current one are accepted on verification. One step (±30s)
	// absorbs clock drift between the server and the user's phone
	// and the few seconds a human takes to type the code, without
	// widening the window enough to materially help an attacker
	// guessing a 6-digit code.
	totpSkewSteps = 1

	// totpSecretBytes is the entropy of a freshly generated TOTP
	// secret. RFC 4226 §4 requires at least 128 bits and recommends
	// 160; 20 bytes (160 bits) matches the SHA-1 block the HMAC uses
	// and is what the RFC's own test vectors use.
	totpSecretBytes = 20
)

// totpEncoding is unpadded, upper-case base32 (RFC 4648) — the encoding
// authenticator apps expect for the secret embedded in an otpauth:// URI
// and for manual entry. Padding ('=') is omitted because the otpauth
// scheme and every app strip it.
var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret returns a fresh base32-encoded TOTP secret with
// totpSecretBytes of entropy. The encoded form is what gets stored and
// embedded in the enrollment URI; the raw bytes never leave this call.
func newTOTPSecret() (string, error) {
	buf := make([]byte, totpSecretBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("console: generate totp secret: %w", err)
	}
	return totpEncoding.EncodeToString(buf), nil
}

// totpStep maps an instant to its RFC 6238 time-step counter
// (floor(unix / period)). It is the moving factor fed to the HOTP core.
func totpStep(t time.Time) int64 {
	return t.Unix() / int64(totpPeriod/time.Second)
}

// hotpCode computes the RFC 4226 HOTP value for the given key and
// counter, truncated to totpDigits and zero-padded to a fixed width. It
// is the shared core of both code generation and verification.
func hotpCode(key []byte, counter int64) string {
	var ctr [8]byte
	binary.BigEndian.PutUint64(ctr[:], uint64(counter))

	mac := hmac.New(sha1.New, key)
	mac.Write(ctr[:])
	sum := mac.Sum(nil)

	// Dynamic truncation (RFC 4226 §5.3): the low nibble of the last
	// byte selects a 4-byte window, whose high bit is masked off to
	// avoid sign ambiguity, then reduced modulo 10^digits.
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod)
}

// totpVerify reports whether code is a valid TOTP for secretB32 at time
// t, scanning ±totpSkewSteps around the current step. On success it
// returns the matched step so the caller can record it and reject a
// replay of the same code within its still-valid window.
//
// Each candidate comparison uses subtle.ConstantTimeCompare so a timing
// side channel cannot reveal how many leading digits were correct — the
// security-relevant guarantee, since the digits are the secret-derived
// value an attacker is guessing. The loop does early-return on the first
// match, so the *total* runtime can betray which of the (2·skew+1) steps
// matched; that leaks only the coarse server↔authenticator clock
// alignment, which an attacker already knows from the wall clock, so it
// is not worth the constant extra HMACs to scan all steps unconditionally.
//
// matchedStep is meaningful only when ok is true.
func totpVerify(secretB32, code string, t time.Time) (matchedStep int64, ok bool) {
	code = strings.TrimSpace(code)
	// A well-formed code is exactly totpDigits ASCII digits. Reject
	// anything else up front so a garbage or wrong-length input can
	// never accidentally collide after truncation.
	if len(code) != totpDigits {
		return 0, false
	}
	if _, err := strconv.Atoi(code); err != nil {
		return 0, false
	}
	key, err := decodeTOTPSecret(secretB32)
	if err != nil {
		return 0, false
	}
	current := totpStep(t)
	for delta := int64(-totpSkewSteps); delta <= totpSkewSteps; delta++ {
		step := current + delta
		candidate := hotpCode(key, step)
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

// decodeTOTPSecret decodes a stored base32 secret back to raw key bytes,
// tolerating the padded or lower-case forms a hand-entered secret might
// arrive in.
func decodeTOTPSecret(secretB32 string) ([]byte, error) {
	s := strings.ToUpper(strings.TrimSpace(secretB32))
	s = strings.TrimRight(s, "=")
	key, err := totpEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("console: decode totp secret: %w", err)
	}
	return key, nil
}

// totpEnrollmentURI builds the otpauth://totp/ URI an authenticator app
// scans from a QR code. issuer is the human-facing service name shown in
// the app; account identifies which login the secret belongs to (the
// user's email). Both are placed in the label (issuer:account) and as
// query parameters per the Key URI Format, so apps that read either
// location display the right names. The explicit algorithm / digits /
// period parameters keep the URI unambiguous even though they are the
// app defaults.
func totpEnrollmentURI(issuer, account, secretB32 string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secretB32)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(int(totpPeriod/time.Second)))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
