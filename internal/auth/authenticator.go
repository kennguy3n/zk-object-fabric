package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// HMACAuthenticator verifies AWS Signature V4 (SigV4) on incoming
// requests and returns the tenant bound to the signing access key.
//
// Authentication is dispatched through a list of AuthStrategy
// implementations registered on the authenticator. The first
// strategy whose Matches returns true is used; if no strategy
// matches the authenticator returns an error indicating no
// recognised SigV4 credential was presented.
//
// The default strategy list is:
//
//   - PresignedV4Strategy: query-string presigned URLs
//     (X-Amz-Signature in the query).
//   - HeaderV4Strategy: header-signed SigV4 requests
//     (Authorization: AWS4-HMAC-SHA256 ...). Also handles
//     aws-chunked streaming uploads by validating the seed
//     signature and returning the signing key so the handler can
//     verify per-chunk signatures (see VerifyChunkSignature).
//
// Future strategies (STS temporary credentials, SigV4A multi-region)
// can be added without modifying the dispatch by appending to
// HMACAuthenticator.Strategies.
type HMACAuthenticator struct {
	Store        TenantStore
	Region       string
	Service      string
	Clock        func() time.Time
	MaxClockSkew time.Duration

	// Strategies is the ordered list of authentication strategies
	// the authenticator will attempt. A nil or empty slice falls
	// back to DefaultStrategies. Callers wanting to add a new
	// strategy (e.g. STSV4Strategy) should append it here.
	Strategies []AuthStrategy
}

// AuthResult is the rich form of an authentication outcome. The
// legacy Authenticate method returns only the tenant ID; callers
// (notably the chunked-upload handler) that need the signing key,
// seed signature, or signing scope should use AuthenticateEx.
type AuthResult struct {
	// TenantID is the tenant bound to the signing access key.
	TenantID string

	// SigningKey is the derived SigV4 signing key. Non-nil only
	// for chunked requests; chunk verifiers feed this key to
	// VerifyChunkSignature to validate the chunk chain.
	SigningKey []byte

	// SeedSig is the seed signature reported in the Authorization
	// header. The first chunk's signature is chained off this
	// value, the second chunk's off the first chunk's signature,
	// and so on.
	SeedSig string

	// IsChunked is true when the request advertises
	// `Content-Encoding: aws-chunked` and the strategy validated
	// the seed signature accordingly.
	IsChunked bool

	// Timestamp is the ISO 8601 SigV4 timestamp
	// (e.g. "20060102T150405Z") used to sign the request. When the
	// Date header fallback was used the timestamp is normalised
	// from RFC1123 to ISO 8601 here.
	Timestamp string

	// Scope is the SigV4 credential scope
	// (e.g. "20060102/us-east-1/s3/aws4_request").
	Scope string
}

// AuthStrategy is one method of authenticating an inbound S3
// request. Implementations are pure: they read from r and store,
// never mutate r, and return a typed AuthResult on success.
type AuthStrategy interface {
	// Name is a stable short identifier used in errors and logs.
	Name() string

	// Matches reports whether this strategy is responsible for r.
	// The first matching strategy in HMACAuthenticator.Strategies
	// is called; subsequent strategies are not consulted, even if
	// the chosen strategy returns an error.
	Matches(r *http.Request) bool

	// Authenticate validates r and returns the AuthResult or an
	// error explaining why the request was rejected.
	Authenticate(r *http.Request, store TenantStore, clock func() time.Time, skew time.Duration) (AuthResult, error)
}

// DefaultStrategies returns a fresh slice of the built-in strategy
// list. Callers extending the strategy list should base their slice
// on this so the default ordering (presigned, then header / chunked)
// is preserved.
func DefaultStrategies() []AuthStrategy {
	return []AuthStrategy{
		PresignedV4Strategy{},
		HeaderV4Strategy{},
	}
}

// NewHMACAuthenticator returns an HMACAuthenticator with sensible
// defaults (region=us-east-1, service=s3, max clock skew=15m) and
// the built-in strategy list.
func NewHMACAuthenticator(store TenantStore) *HMACAuthenticator {
	return &HMACAuthenticator{
		Store:        store,
		Region:       "us-east-1",
		Service:      "s3",
		Clock:        time.Now,
		MaxClockSkew: 15 * time.Minute,
		Strategies:   DefaultStrategies(),
	}
}

// Authenticate implements s3compat.Authenticator. It delegates to
// AuthenticateEx and returns only the tenant ID for backward
// compatibility with code that does not need the chunked-signing
// metadata.
func (a *HMACAuthenticator) Authenticate(r *http.Request) (string, error) {
	res, err := a.AuthenticateEx(r)
	if err != nil {
		return "", err
	}
	return res.TenantID, nil
}

// AuthenticateEx is the rich-result form of Authenticate. It walks
// the configured strategy list in order, dispatching to the first
// strategy whose Matches returns true. If no strategy matches it
// returns an error so callers can distinguish "no credential" from
// "credential rejected".
func (a *HMACAuthenticator) AuthenticateEx(r *http.Request) (AuthResult, error) {
	if a == nil || a.Store == nil {
		return AuthResult{}, errors.New("auth: authenticator not configured")
	}
	clock := a.Clock
	if clock == nil {
		clock = time.Now
	}
	skew := a.MaxClockSkew
	if skew <= 0 {
		skew = 15 * time.Minute
	}
	strategies := a.Strategies
	if len(strategies) == 0 {
		strategies = DefaultStrategies()
	}
	for _, s := range strategies {
		if s.Matches(r) {
			return s.Authenticate(r, a.Store, clock, skew)
		}
	}
	return AuthResult{}, errors.New("auth: no SigV4 credential presented (missing Authorization header or X-Amz-Signature)")
}

// HeaderV4Strategy authenticates header-signed SigV4 requests
// (`Authorization: AWS4-HMAC-SHA256 ...`). It also handles
// aws-chunked streaming uploads: if Content-Encoding contains
// "aws-chunked" the seed signature is validated using the
// STREAMING-AWS4-HMAC-SHA256-PAYLOAD payload hash and the
// AuthResult carries the signing key so the handler can run
// per-chunk signature verification via VerifyChunkSignature.
type HeaderV4Strategy struct{}

// Name returns the strategy's stable identifier.
func (HeaderV4Strategy) Name() string { return "header-v4" }

// Matches reports whether the Authorization header carries a
// SigV4 signature.
func (HeaderV4Strategy) Matches(r *http.Request) bool {
	return strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")
}

// Authenticate validates a header-signed SigV4 request.
func (HeaderV4Strategy) Authenticate(r *http.Request, store TenantStore, clock func() time.Time, skew time.Duration) (AuthResult, error) {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return AuthResult{}, errors.New("auth: missing Authorization header")
	}
	parsed, err := parseAuthHeader(authz)
	if err != nil {
		return AuthResult{}, err
	}
	binding, ok := store.LookupByAccessKey(parsed.AccessKey)
	if !ok {
		return AuthResult{}, errors.New("auth: unknown access key")
	}

	dateHeader, reqTime, err := parseSigningTimestamp(r)
	if err != nil {
		return AuthResult{}, err
	}
	if diff := clock().Sub(reqTime); diff > skew || diff < -skew {
		return AuthResult{}, fmt.Errorf("auth: request clock skew %s exceeds limit", diff)
	}

	// Build a signing clone so the canonical request always sees
	// the timestamp on the x-amz-date header in ISO 8601 form,
	// even when the client supplied only a Date header.
	signingReq := cloneForSigning(r, dateHeader)

	expected, err := signRequest(signingReq, parsed, binding.SecretKey)
	if err != nil {
		return AuthResult{}, err
	}
	if !hmac.Equal([]byte(expected), []byte(parsed.Signature)) {
		return AuthResult{}, errors.New("auth: signature mismatch")
	}

	scope := fmt.Sprintf("%s/%s/%s/aws4_request", parsed.Date, parsed.Region, parsed.Service)
	res := AuthResult{
		TenantID:  binding.Tenant.ID,
		Timestamp: dateHeader,
		Scope:     scope,
	}
	if isChunkedRequest(r) {
		res.IsChunked = true
		res.SeedSig = parsed.Signature
		res.SigningKey = deriveSigningKey(binding.SecretKey, parsed.Date, parsed.Region, parsed.Service)
	}
	return res, nil
}

// PresignedV4Strategy authenticates SigV4 query-string presigned
// URLs (`X-Amz-Signature=...`).
type PresignedV4Strategy struct{}

// Name returns the strategy's stable identifier.
func (PresignedV4Strategy) Name() string { return "presigned-v4" }

// Matches reports whether the request URL carries an X-Amz-Signature
// query parameter.
func (PresignedV4Strategy) Matches(r *http.Request) bool {
	return r.URL != nil && r.URL.Query().Get("X-Amz-Signature") != ""
}

// Authenticate validates a SigV4 presigned URL. The signing
// parameters live in the query string (X-Amz-Algorithm,
// X-Amz-Credential, X-Amz-Date, X-Amz-Expires, X-Amz-SignedHeaders,
// X-Amz-Signature) instead of the Authorization header; the payload
// hash is fixed to "UNSIGNED-PAYLOAD"; and the canonical query must
// exclude X-Amz-Signature itself.
func (PresignedV4Strategy) Authenticate(r *http.Request, store TenantStore, clock func() time.Time, skew time.Duration) (AuthResult, error) {
	q := r.URL.Query()
	if alg := q.Get("X-Amz-Algorithm"); alg != "AWS4-HMAC-SHA256" {
		return AuthResult{}, fmt.Errorf("auth: unsupported presigned algorithm %q", alg)
	}
	cred := q.Get("X-Amz-Credential")
	if cred == "" {
		return AuthResult{}, errors.New("auth: missing X-Amz-Credential")
	}
	segs := strings.Split(cred, "/")
	if len(segs) != 5 || segs[4] != "aws4_request" {
		return AuthResult{}, fmt.Errorf("auth: malformed X-Amz-Credential %q", cred)
	}
	signedHeadersQ := q.Get("X-Amz-SignedHeaders")
	if signedHeadersQ == "" {
		return AuthResult{}, errors.New("auth: missing X-Amz-SignedHeaders")
	}
	signedHeaders := strings.Split(signedHeadersQ, ";")
	sort.Strings(signedHeaders)
	signature := q.Get("X-Amz-Signature")
	dateStr := q.Get("X-Amz-Date")
	if dateStr == "" {
		return AuthResult{}, errors.New("auth: missing X-Amz-Date")
	}
	reqTime, err := time.Parse("20060102T150405Z", dateStr)
	if err != nil {
		return AuthResult{}, fmt.Errorf("auth: invalid X-Amz-Date: %w", err)
	}
	expiresStr := q.Get("X-Amz-Expires")
	if expiresStr == "" {
		return AuthResult{}, errors.New("auth: missing X-Amz-Expires")
	}
	expiresSec, err := strconv.ParseInt(expiresStr, 10, 64)
	if err != nil || expiresSec <= 0 || expiresSec > 604800 {
		return AuthResult{}, fmt.Errorf("auth: invalid X-Amz-Expires %q", expiresStr)
	}

	p := parsedAuthHeader{
		AccessKey:     segs[0],
		Date:          segs[1],
		Region:        segs[2],
		Service:       segs[3],
		SignedHeaders: signedHeaders,
		Signature:     signature,
	}
	binding, ok := store.LookupByAccessKey(p.AccessKey)
	if !ok {
		return AuthResult{}, errors.New("auth: unknown access key")
	}

	now := clock()
	if now.Before(reqTime.Add(-skew)) {
		return AuthResult{}, errors.New("auth: presigned request dated in the future")
	}
	if now.After(reqTime.Add(time.Duration(expiresSec)*time.Second + skew)) {
		return AuthResult{}, errors.New("auth: presigned URL has expired")
	}

	// signRequest reads the timestamp from the x-amz-date header and
	// derives the canonical query from r.URL.RawQuery. Build a
	// minimal clone that strips X-Amz-Signature from the query and
	// exposes the signing timestamp via the header, so the header
	// and presigned paths share signRequest.
	clonedURL := *r.URL
	clonedURL.RawQuery = stripQueryParam(r.URL.RawQuery, "X-Amz-Signature")
	signingReq := &http.Request{
		Method: r.Method,
		Host:   r.Host,
		URL:    &clonedURL,
		Header: r.Header.Clone(),
	}
	if signingReq.Header == nil {
		signingReq.Header = http.Header{}
	}
	signingReq.Header.Set("x-amz-date", dateStr)
	// Presigned URLs always sign with UNSIGNED-PAYLOAD. Force the
	// sentinel so a client-supplied x-amz-content-sha256 header
	// cannot change the canonical request.
	signingReq.Header.Del("x-amz-content-sha256")

	expected, err := signRequest(signingReq, p, binding.SecretKey)
	if err != nil {
		return AuthResult{}, err
	}
	if !hmac.Equal([]byte(expected), []byte(signature)) {
		return AuthResult{}, errors.New("auth: signature mismatch")
	}
	scope := fmt.Sprintf("%s/%s/%s/aws4_request", p.Date, p.Region, p.Service)
	return AuthResult{
		TenantID:  binding.Tenant.ID,
		Timestamp: dateStr,
		Scope:     scope,
	}, nil
}

// parseSigningTimestamp returns the ISO 8601 timestamp the request
// was signed at, falling back from x-amz-date to the standard Date
// header (RFC1123). On success the returned dateHeader is always in
// ISO 8601 form ("20060102T150405Z") even when the client sent only
// a Date header, so callers can place it on the signing request
// clone for canonical-request reconstruction.
func parseSigningTimestamp(r *http.Request) (string, time.Time, error) {
	dateHeader := r.Header.Get("x-amz-date")
	if dateHeader == "" {
		dateHeader = r.Header.Get("Date")
	}
	if dateHeader == "" {
		return "", time.Time{}, errors.New("auth: missing x-amz-date or Date header")
	}
	reqTime, err := time.Parse("20060102T150405Z", dateHeader)
	if err != nil {
		alt, err2 := time.Parse(time.RFC1123, dateHeader)
		if err2 != nil {
			return "", time.Time{}, fmt.Errorf("auth: invalid date header: %w", err)
		}
		reqTime = alt
		dateHeader = reqTime.UTC().Format("20060102T150405Z")
	}
	return dateHeader, reqTime, nil
}

// cloneForSigning returns a shallow clone of r whose x-amz-date
// header is normalised to dateHeader. The clone is used to feed
// signRequest so the canonical request matches regardless of which
// header the client originally used to send the timestamp.
func cloneForSigning(r *http.Request, dateHeader string) *http.Request {
	clone := &http.Request{
		Method: r.Method,
		Host:   r.Host,
		URL:    r.URL,
		Header: r.Header.Clone(),
	}
	if clone.Header == nil {
		clone.Header = http.Header{}
	}
	clone.Header.Set("x-amz-date", dateHeader)
	return clone
}

// isChunkedRequest reports whether r advertises the AWS chunked
// content encoding, indicating the body is a sequence of
// SigV4-signed chunks rather than a single payload hashed at the
// canonical-request level.
func isChunkedRequest(r *http.Request) bool {
	enc := r.Header.Get("Content-Encoding")
	if enc == "" {
		return false
	}
	for _, p := range strings.Split(enc, ",") {
		if strings.EqualFold(strings.TrimSpace(p), "aws-chunked") {
			return true
		}
	}
	return false
}

// VerifyChunkSignature recomputes and compares the signature for a
// single aws-chunked chunk. The chunk-string-to-sign is:
//
//	"AWS4-HMAC-SHA256-PAYLOAD\n" +
//	timestamp + "\n" +
//	scope + "\n" +
//	prevSig + "\n" +
//	hex(sha256("")) + "\n" +
//	hex(sha256(chunkData))
//
// On success the returned signature is the value the next chunk
// must chain off of in its own prevSig field.
//
// This function is a building block for the chunked-upload handler;
// the strategy layer only validates the seed signature in the
// Authorization header. It is exported so the multipart/streaming
// path can call it once per chunk without needing to depend on the
// internal SigV4 helpers.
func VerifyChunkSignature(prevSig string, chunkData []byte, signingKey []byte, timestamp, scope string) (string, error) {
	if len(signingKey) == 0 {
		return "", errors.New("auth: chunk verification requires a signing key")
	}
	if timestamp == "" || scope == "" {
		return "", errors.New("auth: chunk verification requires timestamp and scope")
	}
	emptyHash := sha256.Sum256(nil)
	chunkHash := sha256.Sum256(chunkData)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		timestamp,
		scope,
		prevSig,
		hex.EncodeToString(emptyHash[:]),
		hex.EncodeToString(chunkHash[:]),
	}, "\n")
	return hex.EncodeToString(hmacSHA256(signingKey, stringToSign)), nil
}

// stripQueryParam removes every occurrence of the given parameter
// (matched case-sensitively against the URL-encoded name) from a raw
// query string, preserving the order of the remaining segments.
func stripQueryParam(raw, name string) string {
	if raw == "" {
		return ""
	}
	prefix := name + "="
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, seg := range parts {
		if strings.HasPrefix(seg, prefix) || seg == name {
			continue
		}
		out = append(out, seg)
	}
	return strings.Join(out, "&")
}

// parsedAuthHeader is the structured form of an SigV4 Authorization
// header.
type parsedAuthHeader struct {
	AccessKey       string
	Date            string
	Region          string
	Service         string
	SignedHeaders   []string
	Signature       string
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

	kSigning := deriveSigningKey(secretKey, p.Date, p.Region, p.Service)
	return hex.EncodeToString(hmacSHA256(kSigning, stringToSign)), nil
}

// deriveSigningKey returns the SigV4 signing key for the given
// secret-key / scope tuple. The same key signs the seed signature
// and every per-chunk signature on a chunked upload, so the
// strategy layer derives it once and hands it to the chunked
// handler via AuthResult.SigningKey.
func deriveSigningKey(secretKey, date, region, service string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secretKey), date)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, service)
	return hmacSHA256(kService, "aws4_request")
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
