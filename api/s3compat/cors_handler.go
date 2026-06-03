// WS8.5 — S3 bucket CORS (`?cors`) plus cross-origin response headers.
//
// Implements the bucket-level CORS configuration sub-resource
// (Put/Get/DeleteBucketCors), persisted through metadata/bucket_config,
// and the request-time machinery that makes browser direct-uploads
// work:
//
//   - applyCORS runs on every dispatched request that carries an
//     Origin header. When the bucket has a CORS rule matching the
//     request's origin and method, it sets the Access-Control-* headers
//     on the response so the browser surfaces the result to client
//     script. A request with no matching rule simply gets no CORS
//     headers (the browser then blocks it), matching AWS — it is never
//     an error on the actual request.
//   - handleCORSPreflight answers the OPTIONS preflight a browser sends
//     before a non-simple cross-origin request. Unlike every other S3
//     operation it is unauthenticated, because browsers never attach
//     credentials to a preflight. A preflight with no matching rule is
//     rejected with 403, matching AWS.
//
// CORS only affects browsers; non-browser SDKs never send Origin, so
// the actual-request path skips the store lookup entirely when Origin
// is absent.
package s3compat

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
)

// ---- XML document types ----

// corsRuleXML is a single <CORSRule>. AWS allows repeated
// <AllowedOrigin>/<AllowedMethod>/<AllowedHeader>/<ExposeHeader>
// elements, which decode into slices.
type corsRuleXML struct {
	ID             string   `xml:"ID,omitempty"`
	AllowedOrigins []string `xml:"AllowedOrigin"`
	AllowedMethods []string `xml:"AllowedMethod"`
	AllowedHeaders []string `xml:"AllowedHeader,omitempty"`
	ExposeHeaders  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds  int      `xml:"MaxAgeSeconds,omitempty"`
}

// corsConfiguration is the PUT/GET ?cors body:
//
//	<CORSConfiguration><CORSRule>
//	  <AllowedOrigin>https://app.example.com</AllowedOrigin>
//	  <AllowedMethod>GET</AllowedMethod><AllowedMethod>PUT</AllowedMethod>
//	  <AllowedHeader>*</AllowedHeader>
//	  <ExposeHeader>ETag</ExposeHeader><MaxAgeSeconds>3000</MaxAgeSeconds>
//	</CORSRule></CORSConfiguration>
type corsConfiguration struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	XMLNS   string        `xml:"xmlns,attr,omitempty"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

func corsConfigFromXML(doc corsConfiguration) cors.Config {
	rules := make([]cors.Rule, len(doc.Rules))
	for i, r := range doc.Rules {
		rules[i] = cors.Rule{
			ID:             r.ID,
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAgeSeconds:  r.MaxAgeSeconds,
		}
	}
	return cors.Config{Rules: rules}
}

func corsConfigToXML(cfg cors.Config) corsConfiguration {
	doc := corsConfiguration{XMLNS: s3XMLNamespace, Rules: make([]corsRuleXML, len(cfg.Rules))}
	for i, r := range cfg.Rules {
		doc.Rules[i] = corsRuleXML{
			ID:             r.ID,
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAgeSeconds:  r.MaxAgeSeconds,
		}
	}
	return doc
}

// ---- bucket-level configuration handlers ----

// PutBucketCors handles PUT /{bucket}?cors. It replaces the bucket's
// CORS configuration with the supplied rule set.
func (h *Handler) PutBucketCors(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "cors is a bucket-level sub-resource; path must be /{bucket}?cors", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket CORS is not configured on this gateway", r.URL.Path)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc corsConfiguration
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse CORSConfiguration: "+err.Error(), r.URL.Path)
		return
	}
	cfg := corsConfigFromXML(doc)
	if err := cfg.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.SetCORS(r.Context(), tenantID, bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "CORSPutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketCors handles GET /{bucket}?cors. It returns the bucket's
// CORS configuration, or 404 NoSuchCORSConfiguration when the bucket
// has none, matching AWS.
func (h *Handler) GetBucketCors(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?cors", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket CORS is not configured on this gateway", r.URL.Path)
		return
	}
	cfg, err := h.cfg.BucketConfig.GetCORS(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CORSGetFailed", err.Error(), r.URL.Path)
		return
	}
	if cfg.Empty() {
		writeError(w, http.StatusNotFound, "NoSuchCORSConfiguration", "The CORS configuration does not exist", r.URL.Path)
		return
	}
	writeXMLDoc(w, corsConfigToXML(cfg))
}

// DeleteBucketCors handles DELETE /{bucket}?cors. It removes the
// bucket's CORS configuration and returns 204 No Content. Deleting a
// bucket with no CORS configuration is a no-op success, matching AWS's
// idempotent DeleteBucketCors.
func (h *Handler) DeleteBucketCors(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "cors is a bucket-level sub-resource; path must be /{bucket}?cors", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket CORS is not configured on this gateway", r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.DeleteCORS(r.Context(), tenantID, bucket); err != nil {
		writeError(w, http.StatusInternalServerError, "CORSDeleteFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- cross-origin response headers ----

// bucketCORSRules resolves the bucket's CORS rule set. A nil
// BucketConfig store (feature unwired) reports an empty Config with no
// error, so the caller treats the bucket as having no CORS rules. The
// returned bool is false on any lookup error; the actual-request path
// treats an error as "no CORS headers" rather than failing the
// request, since CORS is advisory and the underlying operation should
// still proceed (the browser will simply block the response).
func (h *Handler) bucketCORSRules(r *http.Request, tenantID, bucket string) (cors.Config, bool) {
	if h.cfg.BucketConfig == nil || bucket == "" {
		return cors.Config{}, false
	}
	cfg, err := h.cfg.BucketConfig.GetCORS(r.Context(), tenantID, bucket)
	if err != nil {
		return cors.Config{}, false
	}
	return cfg, true
}

// applyCORS sets the Access-Control-* response headers for a simple
// (non-preflight) cross-origin request when the bucket has a CORS rule
// matching the request's Origin and method. It is a no-op when the
// request carries no Origin (non-browser client) or no rule matches.
// It must run before the request handler writes its response so the
// headers are present even on error responses, mirroring how AWS
// attaches CORS headers regardless of the operation's outcome.
//
// The caller's identity is resolved with the same authenticate() the
// handlers use. On an auth error the CORS headers are still attached
// (resolving the tenant unverified from the presigned credential, the
// same way the preflight does) so a browser SPA can read the real 403
// the downstream handler writes instead of an opaque CORS error. This
// grants no access — the operation handler still authenticates and
// rejects the request itself.
func (h *Handler) applyCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	// AWS sets Vary: Origin on every response to a request that carries
	// an Origin, whether or not a rule matches, so a shared cache never
	// serves a CORS response to a different origin (or a non-CORS
	// response to a matching one). Emit it up front, before the auth /
	// match short-circuits below.
	w.Header().Add("Vary", "Origin")
	tenantID, err := h.authenticate(r)
	if err != nil {
		// The request failed signature verification, but CORS headers
		// must still attach to the error response. Resolve the tenant
		// from the presigned credential without verifying — identical
		// to the preflight path, and granting no access.
		resolved, ok := h.preflightTenant(r)
		if !ok {
			return
		}
		tenantID = resolved
	}
	bucket, _ := parseBucketKey(r.URL.Path)
	cfg, ok := h.bucketCORSRules(r, tenantID, bucket)
	if !ok || cfg.Empty() {
		return
	}
	rule, matched := cfg.Match(origin, r.Method)
	if !matched {
		return
	}
	// The actual request is where the browser reads Expose-Headers, so
	// include them here (unlike the preflight).
	setCORSResponseHeaders(w, origin, rule, true)
}

// preflightTenant resolves which tenant's bucket CORS rules an
// (unauthenticated) OPTIONS preflight should be matched against:
//
//   - With no Authenticator wired (dev), every request is the
//     AnonymousTenant — the same tenant PutBucketCors stored under.
//     A RequireAuth-without-Authenticator server is misconfigured and
//     cannot resolve any tenant.
//   - With an Authenticator wired (production), a browser never signs
//     the preflight, but a presigned-URL preflight carries
//     X-Amz-Credential naming the tenant; a TenantResolver resolves it
//     without verifying the signature. This grants no access — the
//     follow-up actual request is fully authenticated.
//
// ok is false when no tenant can be resolved, in which case the
// preflight is rejected with 403.
func (h *Handler) preflightTenant(r *http.Request) (string, bool) {
	if h.cfg.Auth == nil {
		return AnonymousTenant, !h.cfg.RequireAuth
	}
	if tr, ok := h.cfg.Auth.(TenantResolver); ok {
		return tr.ResolveTenantUnverified(r)
	}
	return "", false
}

// handleCORSPreflight answers an OPTIONS preflight. It is
// unauthenticated by design (browsers never send credentials on a
// preflight) and is rejected with 403 when CORS is not configured or
// no rule allows the requested origin/method/headers, matching AWS.
func (h *Handler) handleCORSPreflight(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	reqMethod := r.Header.Get("Access-Control-Request-Method")
	// Vary: Origin on every Origin-bearing preflight response, including
	// the 400 below (Origin present but Request-Method absent) and the
	// 403s further down, so caches key on the origin (matches AWS).
	if origin != "" {
		w.Header().Add("Vary", "Origin")
	}
	if origin == "" || reqMethod == "" {
		writeError(w, http.StatusBadRequest, "InvalidCORSRequest", "OPTIONS preflight requires Origin and Access-Control-Request-Method headers", r.URL.Path)
		return
	}
	bucket, _ := parseBucketKey(r.URL.Path)
	if h.cfg.BucketConfig == nil || bucket == "" {
		writeCORSForbidden(w, r)
		return
	}
	// A preflight is unauthenticated (browsers never sign it), so the
	// tenant whose CORS rules apply is resolved from the presigned
	// credential the URL carries rather than a verified signature.
	tenantID, ok := h.preflightTenant(r)
	if !ok {
		writeCORSForbidden(w, r)
		return
	}
	cfg, lookupOK := h.bucketCORSRules(r, tenantID, bucket)
	if !lookupOK || cfg.Empty() {
		writeCORSForbidden(w, r)
		return
	}
	rule, matched := cfg.Match(origin, reqMethod)
	if !matched {
		writeCORSForbidden(w, r)
		return
	}
	reqHeaders := splitRequestHeaders(r.Header.Get("Access-Control-Request-Headers"))
	if !rule.AllowsHeaders(reqHeaders) {
		writeCORSForbidden(w, r)
		return
	}
	// A preflight omits Access-Control-Expose-Headers: the browser only
	// reads it from the actual response, and AWS S3 does not emit it on
	// preflights.
	setCORSResponseHeaders(w, origin, rule, false)
	// Echo the requested headers (already validated as allowed) so the
	// browser permits them on the follow-up actual request.
	if reqHdr := r.Header.Get("Access-Control-Request-Headers"); reqHdr != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqHdr)
	}
	if rule.MaxAgeSeconds > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
	w.WriteHeader(http.StatusOK)
}

// setCORSResponseHeaders writes the Access-Control headers common to a
// matched simple request and a successful preflight. The request
// Origin is echoed (rather than emitting "*"), and
// Access-Control-Allow-Credentials: true is set so credentialed
// requests (fetch credentials:'include' / XHR withCredentials) succeed
// — which requires a specific origin, never "*", as echoed here. AWS S3
// emits this header on every matched response. Vary: Origin is set by
// the callers for all Origin-bearing responses. includeExpose controls
// Access-Control-Expose-Headers, which is meaningful only on the
// actual response (the browser ignores it on a preflight, and AWS S3
// does not send it there).
func setCORSResponseHeaders(w http.ResponseWriter, origin string, rule cors.Rule, includeExpose bool) {
	header := w.Header()
	header.Set("Access-Control-Allow-Origin", origin)
	header.Set("Access-Control-Allow-Credentials", "true")
	header.Set("Access-Control-Allow-Methods", rule.AllowedMethodsCSV())
	if includeExpose {
		if expose := rule.ExposeHeadersCSV(); expose != "" {
			header.Set("Access-Control-Expose-Headers", expose)
		}
	}
}

// writeCORSForbidden emits the 403 AWS returns for a preflight that no
// rule allows.
func writeCORSForbidden(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusForbidden, "AccessForbidden", "CORSResponse: This CORS request is not allowed. This is usually because the evaluation of Origin, request method / Access-Control-Request-Method or Access-Control-Request-Headers are not whitelisted by the resource's CORS spec.", r.URL.Path)
}

// splitRequestHeaders splits a comma-separated
// Access-Control-Request-Headers value into trimmed, non-empty header
// names.
func splitRequestHeaders(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
