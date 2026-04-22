package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// HMACAuthenticator verifies AWS Signature V4 (SigV4) headers on
// incoming requests and returns the tenant bound to the signing
// access key. The implementation follows the SigV4 spec closely
// enough to interoperate with standard S3 SDKs while deliberately
// omitting the parts that have no security value for Phase 2
// (chunked SignatureV4, pre-signed URLs beyond the minimum, and the
// optional x-amz-date fallback); those become live gates in Phase 3.
type HMACAuthenticator struct {
	Store         TenantStore
	Region        string
	Service       string
	Clock         func() time.Time
	MaxClockSkew  time.Duration
}

// NewHMACAuthenticator returns an HMACAuthenticator with sensible
// defaults (region=us-east-1, service=s3, max clock skew=15m).
func NewHMACAuthenticator(store TenantStore) *HMACAuthenticator {
	return &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        time.Now,
		MaxClockSkew: 15 * time.Minute,
	}
}

// Authenticate implements s3compat.Authenticator. It parses the
// Authorization header, re-derives the expected signature using the
// stored secret key, and compares the two in constant time.
func (a *HMACAuthenticator) Authenticate(r *http.Request) (string, error) {
	if a == nil || a.Store == nil {
		return "", errors.New("auth: authenticator not configured")
	}
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return "", errors.New("auth: missing Authorization header")
	}
	parsed, err := parseAuthHeader(authz)
	if err != nil {
		return "", err
	}
	binding, ok := a.Store.LookupByAccessKey(parsed.AccessKey)
	if !ok {
		return "", errors.New("auth: unknown access key")
	}

	dateHeader := r.Header.Get("x-amz-date")
	if dateHeader == "" {
		return "", errors.New("auth: missing x-amz-date header")
	}
	reqTime, err := time.Parse("20060102T150405Z", dateHeader)
	if err != nil {
		return "", fmt.Errorf("auth: invalid x-amz-date: %w", err)
	}
	clock := a.Clock
	if clock == nil {
		clock = time.Now
	}
	skew := a.MaxClockSkew
	if skew <= 0 {
		skew = 15 * time.Minute
	}
	if diff := clock().Sub(reqTime); diff > skew || diff < -skew {
		return "", fmt.Errorf("auth: request clock skew %s exceeds limit", diff)
	}

	expected, err := signRequest(r, parsed, binding.SecretKey)
	if err != nil {
		return "", err
	}
	if !hmac.Equal([]byte(expected), []byte(parsed.Signature)) {
		return "", errors.New("auth: signature mismatch")
	}
	return binding.Tenant.ID, nil
}

// parsedAuthHeader is the structured form of an SigV4 Authorization
// header.
type parsedAuthHeader struct {
	AccessKey     string
	Date          string
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
	CredentialScope string
}

// parseAuthHeader extracts the four fields we care about from the
// SigV4 Authorization header:
//
//	AWS4-HMAC-SHA256 Credential=AKID/20240101/us-east-1/s3/aws4_request,
//	  SignedHeaders=host;x-amz-date, Signature=abcdef...
func parseAuthHeader(authz string) (parsedAuthHeader, error) {
	var p parsedAuthHeader
	const prefix = "AWS4-HMAC-SHA256 "
	if !strings.HasPrefix(authz, prefix) {
		return p, fmt.Errorf("auth: unsupported auth scheme in %q", authz)
	}
	parts := strings.Split(strings.TrimPrefix(authz, prefix), ",")
	for _, raw := range parts {
		kv := strings.SplitN(strings.TrimSpace(raw), "=", 2)
		if len(kv) != 2 {
			return p, fmt.Errorf("auth: malformed auth header segment %q", raw)
		}
		key, val := kv[0], kv[1]
		switch key {
		case "Credential":
			p.CredentialScope = val
			segs := strings.Split(val, "/")
			if len(segs) != 5 {
				return p, fmt.Errorf("auth: malformed credential %q", val)
			}
			p.AccessKey = segs[0]
			p.Date = segs[1]
			p.Region = segs[2]
			p.Service = segs[3]
		case "SignedHeaders":
			p.SignedHeaders = strings.Split(val, ";")
			sort.Strings(p.SignedHeaders)
		case "Signature":
			p.Signature = val
		}
	}
	if p.AccessKey == "" || p.Signature == "" || len(p.SignedHeaders) == 0 {
		return p, errors.New("auth: authorization header is missing required fields")
	}
	return p, nil
}

// signRequest recomputes the SigV4 signature for r using the given
// secret key. Returns the hex-encoded signature.
func signRequest(r *http.Request, p parsedAuthHeader, secretKey string) (string, error) {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	if host == "" {
		return "", errors.New("auth: request is missing host")
	}

	canonicalHeaders, err := buildCanonicalHeaders(r, p.SignedHeaders, host)
	if err != nil {
		return "", err
	}
	payloadHash := r.Header.Get("x-amz-content-sha256")
	if payloadHash == "" {
		// Pure SigV4 requires the hash. Accept the unsigned-payload
		// sentinel used by some SDKs; everything else must match
		// what the client advertised.
		payloadHash = "UNSIGNED-PAYLOAD"
	}

	canonicalRequest := strings.Join([]string{
		r.Method,
		canonicalURI(r.URL.Path),
		canonicalQuery(r.URL.RawQuery),
		canonicalHeaders,
		strings.Join(p.SignedHeaders, ";"),
		payloadHash,
	}, "\n")

	sha := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		r.Header.Get("x-amz-date"),
		fmt.Sprintf("%s/%s/%s/aws4_request", p.Date, p.Region, p.Service),
		hex.EncodeToString(sha[:]),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), p.Date)
	kRegion := hmacSHA256(kDate, p.Region)
	kService := hmacSHA256(kRegion, p.Service)
	kSigning := hmacSHA256(kService, "aws4_request")
	return hex.EncodeToString(hmacSHA256(kSigning, stringToSign)), nil
}

func buildCanonicalHeaders(r *http.Request, signedHeaders []string, host string) (string, error) {
	var b strings.Builder
	for _, name := range signedHeaders {
		var value string
		switch strings.ToLower(name) {
		case "host":
			value = host
		default:
			value = r.Header.Get(name)
		}
		b.WriteString(strings.ToLower(name))
		b.WriteByte(':')
		b.WriteString(strings.TrimSpace(value))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// canonicalQuery sorts query parameters lexicographically and
// re-joins them in the form required by SigV4. The empty query
// string produces an empty canonical form.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	sort.Strings(parts)
	return strings.Join(parts, "&")
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
