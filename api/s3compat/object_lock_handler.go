// WS8.3 — S3 Object Lock / WORM (`?object-lock`, `?retention`,
// `?legal-hold`).
//
// Implements the bucket-level Object Lock configuration
// (Put/GetObjectLockConfiguration) plus the per-object retention
// (Put/GetObjectRetention) and legal-hold (Put/GetObjectLegalHold)
// sub-resources. Bucket-level config is persisted through
// metadata/bucket_config; per-object retention and legal hold live on
// the object manifest (metadata.ObjectManifest.RetentionMode /
// RetainUntil / LegalHold) so they version with the object and are
// amended in place via UpdateManifest (never promoting a non-latest
// version, mirroring object tagging — see tagging_handler.go).
//
// Enforcement lives in the DELETE and PUT-overwrite paths
// (handler.go): a version that is under an active retention or a legal
// hold cannot be permanently deleted or overwritten. GOVERNANCE
// retention can be bypassed with the
// x-amz-bypass-governance-retention:true header; COMPLIANCE cannot.
//
// Object Lock requires bucket versioning (it protects versions from
// being silently replaced), so enabling it on a bucket whose
// versioning is not Enabled is rejected. See docs/PROPOSAL.md §15.3.
package s3compat

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
)

// errInvalidRetainUntil is returned when a RetainUntilDate is missing
// or not a valid RFC3339 timestamp.
var errInvalidRetainUntil = errors.New("object_lock: RetainUntilDate must be a valid RFC3339 timestamp")

// bypassGovernanceHeader is the AWS header a caller sets to shorten or
// remove GOVERNANCE-mode retention (and delete a GOVERNANCE-locked
// version). It has no effect on COMPLIANCE-mode retention.
const bypassGovernanceHeader = "x-amz-bypass-governance-retention"

// ---- XML document types ----

// defaultRetention is the <DefaultRetention> element of a bucket
// ObjectLockConfiguration's <Rule>. Exactly one of Days/Years is set.
type defaultRetention struct {
	Mode  string `xml:"Mode,omitempty"`
	Days  int    `xml:"Days,omitempty"`
	Years int    `xml:"Years,omitempty"`
}

type objectLockRule struct {
	DefaultRetention *defaultRetention `xml:"DefaultRetention,omitempty"`
}

// objectLockConfiguration is the PUT/GET ?object-lock body:
//
//	<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>
//	  <Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>30</Days></DefaultRetention></Rule>
//	</ObjectLockConfiguration>
type objectLockConfiguration struct {
	XMLName           xml.Name        `xml:"ObjectLockConfiguration"`
	XMLNS             string          `xml:"xmlns,attr,omitempty"`
	ObjectLockEnabled string          `xml:"ObjectLockEnabled,omitempty"`
	Rule              *objectLockRule `xml:"Rule,omitempty"`
}

// retentionDocument is the PUT/GET ?retention body. RetainUntilDate is
// kept as a string so we control RFC3339 parse/format precisely.
type retentionDocument struct {
	XMLName         xml.Name `xml:"Retention"`
	XMLNS           string   `xml:"xmlns,attr,omitempty"`
	Mode            string   `xml:"Mode,omitempty"`
	RetainUntilDate string   `xml:"RetainUntilDate,omitempty"`
}

// legalHoldDocument is the PUT/GET ?legal-hold body.
type legalHoldDocument struct {
	XMLName xml.Name `xml:"LegalHold"`
	XMLNS   string   `xml:"xmlns,attr,omitempty"`
	Status  string   `xml:"Status,omitempty"`
}

// bucketObjectLock resolves the bucket-level Object Lock config. A nil
// BucketConfig store (feature unwired) reports the zero Config with no
// error so callers treat the bucket as having no Object Lock.
func (h *Handler) bucketObjectLock(r *http.Request, tenantID, bucket string) (object_lock.Config, error) {
	if h.cfg.BucketConfig == nil {
		return object_lock.Config{}, nil
	}
	return h.cfg.BucketConfig.GetObjectLock(r.Context(), tenantID, bucket)
}

// ---- bucket-level configuration ----

// PutObjectLockConfiguration handles PUT /{bucket}?object-lock. It
// enables Object Lock on the bucket (optionally with a default
// retention rule). Object Lock requires versioning to be Enabled.
func (h *Handler) PutObjectLockConfiguration(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "object-lock is a bucket-level sub-resource; path must be /{bucket}?object-lock", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "object lock is not configured on this gateway", r.URL.Path)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc objectLockConfiguration
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse ObjectLockConfiguration: "+err.Error(), r.URL.Path)
		return
	}
	if doc.ObjectLockEnabled != "Enabled" {
		// AWS only accepts ObjectLockEnabled=Enabled; there is no way
		// to express "disable Object Lock" through this API.
		writeError(w, http.StatusBadRequest, "MalformedXML", "ObjectLockEnabled must be 'Enabled'", r.URL.Path)
		return
	}

	cfg := object_lock.Config{Enabled: true}
	if doc.Rule != nil && doc.Rule.DefaultRetention != nil {
		dr := doc.Rule.DefaultRetention
		cfg.DefaultMode = object_lock.RetentionMode(dr.Mode)
		cfg.DefaultDays = dr.Days
		cfg.DefaultYears = dr.Years
	}
	if err := cfg.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error(), r.URL.Path)
		return
	}

	// Object Lock protects versions from silent replacement, so it can
	// only be enabled on a versioning-enabled bucket (AWS auto-enables
	// versioning; this gateway requires it to already be Enabled and
	// reports the dependency rather than mutating versioning as a side
	// effect).
	versioning, verr := h.bucketVersioning(r.Context(), tenantID, bucket)
	if verr != nil {
		writeError(w, http.StatusInternalServerError, "VersioningLookupFailed", verr.Error(), r.URL.Path)
		return
	}
	if versioning != bucket_config.VersioningEnabled {
		writeError(w, http.StatusConflict, "InvalidBucketState", "Object Lock requires bucket versioning to be enabled", r.URL.Path)
		return
	}

	if err := h.cfg.BucketConfig.SetObjectLock(r.Context(), tenantID, bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "ObjectLockPutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetObjectLockConfiguration handles GET /{bucket}?object-lock. It
// returns the bucket's Object Lock configuration, or 404
// ObjectLockConfigurationNotFoundError when the bucket has none, both
// matching AWS.
func (h *Handler) GetObjectLockConfiguration(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?object-lock", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "object lock is not configured on this gateway", r.URL.Path)
		return
	}
	cfg, err := h.cfg.BucketConfig.GetObjectLock(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return
	}
	if !cfg.Enabled {
		writeError(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket", r.URL.Path)
		return
	}
	doc := objectLockConfiguration{XMLNS: s3XMLNamespace, ObjectLockEnabled: "Enabled"}
	if cfg.HasDefaultRetention() {
		doc.Rule = &objectLockRule{DefaultRetention: &defaultRetention{
			Mode:  string(cfg.DefaultMode),
			Days:  cfg.DefaultDays,
			Years: cfg.DefaultYears,
		}}
	}
	writeXMLDoc(w, doc)
}

// ---- per-object retention ----

// PutObjectRetention handles PUT /{bucket}/{key}?retention. It sets the
// retention mode + retain-until date on a specific object version
// (latest, or ?versionId=). Shortening or removing an existing
// COMPLIANCE retention is always refused; GOVERNANCE requires the
// bypass header.
func (h *Handler) PutObjectRetention(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectLock(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc retentionDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse Retention: "+err.Error(), r.URL.Path)
		return
	}
	until, perr := parseRetainUntil(doc.RetainUntilDate)
	if perr != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", perr.Error(), r.URL.Path)
		return
	}
	newRet := object_lock.Retention{Mode: object_lock.RetentionMode(doc.Mode), RetainUntil: until}
	if err := newRet.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error(), r.URL.Path)
		return
	}

	manifest, mkey, ok := h.loadObjectVersion(w, r, tenantID, bucket, key)
	if !ok {
		return
	}

	// Guard against weakening an in-force retention. COMPLIANCE is
	// absolute; GOVERNANCE can be weakened only with the bypass header.
	existing := manifestRetention(manifest)
	now := h.cfg.Now()
	if existing.Active(now) && weakensRetention(existing, newRet) {
		switch existing.Mode {
		case object_lock.ModeCompliance:
			writeError(w, http.StatusForbidden, "AccessDenied", "cannot shorten or remove COMPLIANCE-mode retention before it expires", r.URL.Path)
			return
		case object_lock.ModeGovernance:
			if !governanceBypassRequested(r) {
				writeError(w, http.StatusForbidden, "AccessDenied", "shortening GOVERNANCE-mode retention requires x-amz-bypass-governance-retention:true", r.URL.Path)
				return
			}
		}
	}

	manifest.RetentionMode = string(newRet.Mode)
	manifest.RetainUntil = newRet.RetainUntil
	if err := h.cfg.Manifests.UpdateManifest(r.Context(), mkey, manifest); err != nil {
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

// GetObjectRetention handles GET /{bucket}/{key}?retention. It returns
// the version's retention, or 404 NoSuchObjectLockConfiguration when
// the version has none.
func (h *Handler) GetObjectRetention(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectLock(w, r)
	if !ok {
		return
	}
	manifest, _, ok := h.loadObjectVersion(w, r, tenantID, bucket, key)
	if !ok {
		return
	}
	ret := manifestRetention(manifest)
	if !ret.Mode.Valid() || ret.RetainUntil.IsZero() {
		writeError(w, http.StatusNotFound, "NoSuchObjectLockConfiguration", "the specified object does not have a retention configuration", r.URL.Path)
		return
	}
	doc := retentionDocument{
		XMLNS:           s3XMLNamespace,
		Mode:            string(ret.Mode),
		RetainUntilDate: ret.RetainUntil.UTC().Format(time.RFC3339),
	}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	writeXMLDoc(w, doc)
}

// ---- per-object legal hold ----

// PutObjectLegalHold handles PUT /{bucket}/{key}?legal-hold. It turns
// the legal hold on a specific object version ON or OFF.
func (h *Handler) PutObjectLegalHold(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectLock(w, r)
	if !ok {
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc legalHoldDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse LegalHold: "+err.Error(), r.URL.Path)
		return
	}
	status := object_lock.LegalHoldStatus(doc.Status)
	if !status.Valid() {
		writeError(w, http.StatusBadRequest, "MalformedXML", "LegalHold Status must be ON or OFF", r.URL.Path)
		return
	}

	manifest, mkey, ok := h.loadObjectVersion(w, r, tenantID, bucket, key)
	if !ok {
		return
	}
	manifest.LegalHold = status == object_lock.LegalHoldOn
	if err := h.cfg.Manifests.UpdateManifest(r.Context(), mkey, manifest); err != nil {
		writeError(w, http.StatusInternalServerError, "ManifestPutFailed", err.Error(), r.URL.Path)
		return
	}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

// GetObjectLegalHold handles GET /{bucket}/{key}?legal-hold. It returns
// the version's legal-hold status (OFF when none is set, matching AWS).
func (h *Handler) GetObjectLegalHold(w http.ResponseWriter, r *http.Request) {
	tenantID, bucket, key, ok := h.authObjectLock(w, r)
	if !ok {
		return
	}
	manifest, _, ok := h.loadObjectVersion(w, r, tenantID, bucket, key)
	if !ok {
		return
	}
	status := object_lock.LegalHoldOff
	if manifest.LegalHold {
		status = object_lock.LegalHoldOn
	}
	doc := legalHoldDocument{XMLNS: s3XMLNamespace, Status: string(status)}
	if manifest.VersionID != "" {
		w.Header().Set("x-amz-version-id", manifest.VersionID)
	}
	writeXMLDoc(w, doc)
}

// ---- shared helpers ----

// authObjectLock is the shared preamble for the per-object retention
// and legal-hold handlers: authenticate, require a manifest store,
// parse the path (rejecting bucket-level use), and require that the
// bucket has Object Lock enabled. On failure it writes the response
// and returns ok=false.
func (h *Handler) authObjectLock(w http.ResponseWriter, r *http.Request) (tenantID, bucket, key string, ok bool) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return "", "", "", false
	}
	if h.cfg.Manifests == nil {
		writeError(w, http.StatusServiceUnavailable, "ServiceUnavailable", "manifest store not configured", r.URL.Path)
		return "", "", "", false
	}
	bucket, key = parseBucketKey(r.URL.Path)
	if bucket == "" || key == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "retention and legal-hold are object-level sub-resources; path must be /{bucket}/{key...}", r.URL.Path)
		return "", "", "", false
	}
	cfg, err := h.bucketObjectLock(r, tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return "", "", "", false
	}
	if !cfg.Enabled {
		// AWS rejects per-object retention/legal-hold on buckets
		// without Object Lock enabled.
		writeError(w, http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration", r.URL.Path)
		return "", "", "", false
	}
	return tenantID, bucket, key, true
}

// loadObjectVersion fetches the manifest for the addressed version
// (latest, or ?versionId=) and returns the in-place-amend ManifestKey
// (pinned to the resolved VersionID so UpdateManifest never promotes a
// non-latest version). On failure it writes the response and returns
// ok=false.
func (h *Handler) loadObjectVersion(w http.ResponseWriter, r *http.Request, tenantID, bucket, key string) (*metadata.ObjectManifest, manifest_store.ManifestKey, bool) {
	mkey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     r.URL.Query().Get("versionId"),
	}
	manifest, err := h.cfg.Manifests.Get(r.Context(), mkey)
	if err != nil {
		writeManifestGetError(w, r, err)
		return nil, manifest_store.ManifestKey{}, false
	}
	if manifest.DeleteMarker {
		// A delete marker has no payload to lock; mirror GET/HEAD.
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "the specified version is a delete marker", r.URL.Path)
		return nil, manifest_store.ManifestKey{}, false
	}
	amendKey := manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
		VersionID:     manifest.VersionID,
	}
	return manifest, amendKey, true
}

// manifestRetention reads the Object Lock retention off a manifest.
func manifestRetention(m *metadata.ObjectManifest) object_lock.Retention {
	return object_lock.Retention{
		Mode:        object_lock.RetentionMode(m.RetentionMode),
		RetainUntil: m.RetainUntil,
	}
}

// objectLockBlocksDelete reports whether a permanent delete (or
// overwrite) of this object version must be refused under Object Lock,
// and the S3 message to return. A legal hold blocks unconditionally. A
// COMPLIANCE retention that is still in force blocks unconditionally. A
// GOVERNANCE retention that is still in force blocks unless the caller
// passed x-amz-bypass-governance-retention:true (bypassGovernance).
func objectLockBlocksDelete(m *metadata.ObjectManifest, now time.Time, bypassGovernance bool) (string, bool) {
	if m.LegalHold {
		return "object version is under an active legal hold", true
	}
	ret := manifestRetention(m)
	if !ret.Active(now) {
		return "", false
	}
	if ret.Mode == object_lock.ModeGovernance && bypassGovernance {
		return "", false
	}
	return "object version is protected by an active Object Lock retention", true
}

// allowObjectLockOverwrite is the PUT-path pre-flight for Object Lock.
// It returns true (proceed) for the common case and false (with the
// response already written) when the in-place overwrite must be
// refused. The check is bounded: it is skipped entirely unless the
// gateway has a BucketConfig store wired AND the bucket has Object
// Lock enabled AND versioning is not Enabled (an Enabled bucket writes
// a new version, never overwriting the locked one).
func (h *Handler) allowObjectLockOverwrite(w http.ResponseWriter, r *http.Request, tenantID, bucket, key string) bool {
	if h.cfg.BucketConfig == nil {
		return true
	}
	cfg, err := h.cfg.BucketConfig.GetObjectLock(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ObjectLockGetFailed", err.Error(), r.URL.Path)
		return false
	}
	if !cfg.Enabled {
		return true
	}
	versioning, verr := h.bucketVersioning(r.Context(), tenantID, bucket)
	if verr != nil {
		writeError(w, http.StatusInternalServerError, "VersioningLookupFailed", verr.Error(), r.URL.Path)
		return false
	}
	if versioning == bucket_config.VersioningEnabled {
		return true
	}
	current, err := h.cfg.Manifests.Get(r.Context(), manifest_store.ManifestKey{
		TenantID:      tenantID,
		Bucket:        bucket,
		ObjectKeyHash: hashObjectKey(key),
	})
	if err != nil {
		// No current version (or it cannot be read) → nothing to
		// protect; let the write proceed and surface real store
		// failures on the write path.
		return true
	}
	if msg, locked := objectLockBlocksDelete(current, h.cfg.Now(), governanceBypassRequested(r)); locked {
		writeError(w, http.StatusForbidden, "AccessDenied", msg, r.URL.Path)
		return false
	}
	return true
}

// applyDefaultObjectLockRetention stamps the bucket's default Object
// Lock retention onto a freshly-built manifest when the bucket has a
// default rule and the manifest carries no explicit retention yet, so
// new object versions inherit the default at PUT time (matching AWS).
// It is a no-op when BucketConfig is unwired, Object Lock is off, the
// bucket has no default rule, the manifest already has retention, or
// the manifest is a delete marker.
func (h *Handler) applyDefaultObjectLockRetention(ctx context.Context, tenantID, bucket string, m *metadata.ObjectManifest) error {
	if h.cfg.BucketConfig == nil || m == nil || m.DeleteMarker {
		return nil
	}
	if m.RetentionMode != "" {
		return nil // explicit retention already set; never override
	}
	cfg, err := h.cfg.BucketConfig.GetObjectLock(ctx, tenantID, bucket)
	if err != nil {
		return err
	}
	if !cfg.HasDefaultRetention() {
		return nil
	}
	m.RetentionMode = string(cfg.DefaultMode)
	m.RetainUntil = cfg.DefaultRetainUntil(h.cfg.Now())
	return nil
}

// weakensRetention reports whether applying next would shorten or
// remove the protection currently provided by cur. Extending the
// retain-until date or keeping it equal is always allowed.
func weakensRetention(cur, next object_lock.Retention) bool {
	if !next.Mode.Valid() {
		return true // removing retention entirely
	}
	return next.RetainUntil.Before(cur.RetainUntil)
}

// governanceBypassRequested reports whether the request carries the
// x-amz-bypass-governance-retention:true header.
func governanceBypassRequested(r *http.Request) bool {
	return r.Header.Get(bypassGovernanceHeader) == "true"
}

// parseRetainUntil parses an S3 RetainUntilDate. AWS emits RFC3339
// (sometimes with sub-second precision), so accept both.
func parseRetainUntil(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errInvalidRetainUntil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, errInvalidRetainUntil
	}
	return t, nil
}

// writeXMLDoc serialises an S3 configuration document with the
// standard XML header and content type, returning 200 OK.
func writeXMLDoc(w http.ResponseWriter, doc any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return
	}
	_ = xml.NewEncoder(w).Encode(doc)
}
