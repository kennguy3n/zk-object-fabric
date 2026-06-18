// Package cors defines the provider-neutral domain types for S3
// bucket CORS (Cross-Origin Resource Sharing) configuration:
// the per-bucket set of CORS rules and the matching logic the gateway
// uses to decide which Access-Control-* response headers to emit for a
// cross-origin browser request (and how to answer an OPTIONS
// preflight).
//
// It carries no persistence of its own — bucket-level Config is stored
// through metadata/bucket_config (the per-bucket S3 config store,
// shared with versioning and Object Lock). The api/s3compat layer maps
// between this type and the <CORSConfiguration> XML document and
// applies the matched rule to the HTTP response; this package only
// owns the value semantics (valid rules, origin/method/header
// matching).
package cors

import (
	"errors"
	"fmt"
	"strings"
)

// maxRules is the S3 limit on the number of CORS rules per bucket.
const maxRules = 100

// validMethods is the set of HTTP methods S3 permits in a CORS rule's
// <AllowedMethod>. Anything else is rejected by PutBucketCors.
var validMethods = map[string]bool{
	"GET":    true,
	"PUT":    true,
	"HEAD":   true,
	"POST":   true,
	"DELETE": true,
}

// Rule is a single S3 CORS rule (<CORSRule>). A request matches the
// rule when its Origin matches one of AllowedOrigins and its method is
// in AllowedMethods; for a preflight the requested headers must each
// be covered by AllowedHeaders.
type Rule struct {
	// ID is an optional, opaque rule identifier (<ID>). AWS limits
	// it to 255 characters; it has no matching semantics.
	ID string
	// AllowedOrigins are the origins the rule applies to. Each entry
	// may contain at most one '*' wildcard (e.g. "https://*.example.com"
	// or "*").
	AllowedOrigins []string
	// AllowedMethods are the HTTP methods the rule allows. Each must
	// be one of GET/PUT/HEAD/POST/DELETE.
	AllowedMethods []string
	// AllowedHeaders are the request headers a preflight may ask for.
	// Each entry may contain at most one '*' wildcard. Matching is
	// case-insensitive (HTTP header names are case-insensitive).
	AllowedHeaders []string
	// ExposeHeaders are response headers the browser is allowed to
	// surface to client script (Access-Control-Expose-Headers).
	ExposeHeaders []string
	// MaxAgeSeconds is how long a preflight result may be cached
	// (Access-Control-Max-Age). Zero means the header is omitted.
	MaxAgeSeconds int
}

// Config is the bucket-level CORS configuration set by PutBucketCors.
// An empty Config (no rules) means the bucket has no CORS
// configuration, which GetBucketCors surfaces as 404.
type Config struct {
	Rules []Rule
}

// Empty reports whether the bucket has no CORS rules configured.
func (c Config) Empty() bool {
	return len(c.Rules) == 0
}

// Valid checks that the configuration is well-formed per the S3 CORS
// contract. It is called by PutBucketCors before persisting.
func (c Config) Valid() error {
	if len(c.Rules) == 0 {
		return errors.New("cors: configuration must contain at least one CORSRule")
	}
	if len(c.Rules) > maxRules {
		return fmt.Errorf("cors: at most %d CORSRules are allowed", maxRules)
	}
	for i, r := range c.Rules {
		if err := r.valid(); err != nil {
			return fmt.Errorf("cors: rule %d: %w", i, err)
		}
	}
	return nil
}

func (r Rule) valid() error {
	if len(r.AllowedOrigins) == 0 {
		return errors.New("at least one AllowedOrigin is required")
	}
	if len(r.AllowedMethods) == 0 {
		return errors.New("at least one AllowedMethod is required")
	}
	for _, o := range r.AllowedOrigins {
		if o == "" {
			return errors.New("AllowedOrigin must not be empty")
		}
		if strings.Count(o, "*") > 1 {
			return fmt.Errorf("AllowedOrigin %q may contain at most one wildcard", o)
		}
	}
	for _, m := range r.AllowedMethods {
		if !validMethods[m] {
			return fmt.Errorf("AllowedMethod %q must be one of GET, PUT, HEAD, POST, DELETE", m)
		}
	}
	for _, h := range r.AllowedHeaders {
		if strings.Count(h, "*") > 1 {
			return fmt.Errorf("AllowedHeader %q may contain at most one wildcard", h)
		}
	}
	if len(r.ID) > 255 {
		return errors.New("ID must be at most 255 characters")
	}
	if r.MaxAgeSeconds < 0 {
		return errors.New("MaxAgeSeconds must not be negative")
	}
	return nil
}

// Match returns the first rule whose AllowedOrigins matches origin and
// whose AllowedMethods includes method, mirroring how S3 evaluates
// CORS rules in declaration order. method is the actual request method
// for a simple request, or the Access-Control-Request-Method value for
// a preflight. The returned bool is false when no rule matches.
func (c Config) Match(origin, method string) (Rule, bool) {
	for _, r := range c.Rules {
		if r.matchesOrigin(origin) && r.allowsMethod(method) {
			return r, true
		}
	}
	return Rule{}, false
}

func (r Rule) allowsMethod(method string) bool {
	for _, m := range r.AllowedMethods {
		if m == method {
			return true
		}
	}
	return false
}

func (r Rule) matchesOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	for _, pattern := range r.AllowedOrigins {
		if originMatches(pattern, origin) {
			return true
		}
	}
	return false
}

// AllowsHeaders reports whether every requested header (the
// comma-separated Access-Control-Request-Headers of a preflight) is
// covered by the rule's AllowedHeaders. An empty request set always
// matches. Comparison is case-insensitive.
func (r Rule) AllowsHeaders(requested []string) bool {
	for _, h := range requested {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if !r.allowsHeader(h) {
			return false
		}
	}
	return true
}

func (r Rule) allowsHeader(header string) bool {
	for _, pattern := range r.AllowedHeaders {
		if headerMatches(pattern, header) {
			return true
		}
	}
	return false
}

// AllowedMethodsCSV joins the rule's allowed methods for the
// Access-Control-Allow-Methods response header.
func (r Rule) AllowedMethodsCSV() string {
	return strings.Join(r.AllowedMethods, ", ")
}

// ExposeHeadersCSV joins the rule's expose headers for the
// Access-Control-Expose-Headers response header. Empty when none.
func (r Rule) ExposeHeadersCSV() string {
	return strings.Join(r.ExposeHeaders, ", ")
}

// originMatches reports whether origin satisfies an AllowedOrigin
// pattern. A pattern with no '*' must match exactly; a pattern with a
// single '*' matches when origin shares the literal prefix and suffix
// around the wildcard. A bare "*" matches any origin. Origins are
// compared verbatim (case-sensitively), as the AWS SDKs send them.
func originMatches(pattern, origin string) bool {
	return wildcardMatch(pattern, origin)
}

// headerMatches reports whether a requested header satisfies an
// AllowedHeader pattern. HTTP header names are case-insensitive, so
// both sides are lower-cased before matching.
func headerMatches(pattern, header string) bool {
	return wildcardMatch(strings.ToLower(pattern), strings.ToLower(header))
}

// wildcardMatch implements S3's single-'*' wildcard match. When the
// pattern has no wildcard it must equal s. With one '*' the text
// before and after the wildcard must be a prefix and suffix of s, and
// the wildcard must cover the remainder.
func wildcardMatch(pattern, s string) bool {
	star := strings.IndexByte(pattern, '*')
	if star < 0 {
		return pattern == s
	}
	prefix := pattern[:star]
	suffix := pattern[star+1:]
	if len(s) < len(prefix)+len(suffix) {
		return false
	}
	return strings.HasPrefix(s, prefix) && strings.HasSuffix(s, suffix)
}
