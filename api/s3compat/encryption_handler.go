// S3 bucket default server-side encryption (`?encryption`).
//
// Implements the bucket-level default-encryption sub-resource
// (Put/Get/DeleteBucketEncryption), persisted through
// metadata/bucket_config alongside versioning, Object Lock, CORS, and
// lifecycle. The configuration sets the SSE algorithm applied by
// default to objects written without an explicit
// x-amz-server-side-encryption header.
//
// AWS exposes two bucket-default algorithms — "AES256" (SSE-S3) and
// "aws:kms" (SSE-KMS) — both of which map to ZKOF's gateway-side
// ManagedEncrypted mode (the gateway holds the DEK-wrapping key and
// encrypts on the tenant's behalf). The mapping from algorithm to ZKOF
// mode lives in effectiveEncryptionMode below; this file owns the
// <ServerSideEncryptionConfiguration> XML wire format, the store
// round-trip, and the write-path mode layering.
package s3compat

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/sse"
)

// errEncryptionNotConfigured is returned by effectiveEncryptionMode
// when a bucket has a default-encryption configuration but the gateway
// has no keyring to honor it (a post-config misconfiguration). The
// write path maps it to a clear 500 rather than storing plaintext.
var errEncryptionNotConfigured = errors.New("s3compat: bucket has a default-encryption configuration but gateway encryption is not configured")

// ---- XML document types ----

// sseByDefaultXML is <ApplyServerSideEncryptionByDefault>.
type sseByDefaultXML struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}

// sseRuleXML is a single <Rule>. AWS permits exactly one rule.
type sseRuleXML struct {
	ApplyByDefault   sseByDefaultXML `xml:"ApplyServerSideEncryptionByDefault"`
	BucketKeyEnabled bool            `xml:"BucketKeyEnabled,omitempty"`
}

// serverSideEncryptionConfiguration is the PUT/GET ?encryption body:
//
//<ServerSideEncryptionConfiguration><Rule>
//  <ApplyServerSideEncryptionByDefault>
//    <SSEAlgorithm>aws:kms</SSEAlgorithm>
//    <KMSMasterKeyID>arn:aws:kms:…</KMSMasterKeyID>
//  </ApplyServerSideEncryptionByDefault>
//  <BucketKeyEnabled>true</BucketKeyEnabled>
//</Rule></ServerSideEncryptionConfiguration>
type serverSideEncryptionConfiguration struct {
	XMLName xml.Name     `xml:"ServerSideEncryptionConfiguration"`
	XMLNS   string       `xml:"xmlns,attr,omitempty"`
	Rules   []sseRuleXML `xml:"Rule"`
}

func sseConfigToXML(cfg sse.Config) serverSideEncryptionConfiguration {
	return serverSideEncryptionConfiguration{
		XMLNS: s3XMLNamespace,
		Rules: []sseRuleXML{{
			ApplyByDefault: sseByDefaultXML{
				SSEAlgorithm:   string(cfg.Algorithm),
				KMSMasterKeyID: cfg.KMSMasterKeyID,
			},
			BucketKeyEnabled: cfg.BucketKeyEnabled,
		}},
	}
}

// ---- bucket-level configuration handlers ----

// PutBucketEncryption handles PUT /{bucket}?encryption. It replaces the
// bucket's default server-side-encryption configuration. AWS requires
// exactly one rule; documents with zero or more than one are rejected.
func (h *Handler) PutBucketEncryption(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "encryption is a bucket-level sub-resource; path must be /{bucket}?encryption", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket default encryption is not configured on this gateway", r.URL.Path)
		return
	}
	// Fail closed: both AES256 and aws:kms map to ZKOF gateway-side
	// managed encryption, which needs a configured keyring. Reject the
	// configuration up front rather than accepting a default the
	// gateway cannot honor (which would otherwise surface as a 500 on
	// every subsequent object PUT).
	if h.cfg.Encryption == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "gateway-side encryption is not configured; cannot honor a bucket default-encryption configuration", r.URL.Path)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc serverSideEncryptionConfiguration
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse ServerSideEncryptionConfiguration: "+err.Error(), r.URL.Path)
		return
	}
	if len(doc.Rules) != 1 {
		writeError(w, http.StatusBadRequest, "MalformedXML", "ServerSideEncryptionConfiguration must contain exactly one Rule", r.URL.Path)
		return
	}
	rule := doc.Rules[0]
	cfg := sse.Config{
		Algorithm:        sse.Algorithm(rule.ApplyByDefault.SSEAlgorithm),
		KMSMasterKeyID:   rule.ApplyByDefault.KMSMasterKeyID,
		BucketKeyEnabled: rule.BucketKeyEnabled,
	}
	if err := cfg.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.SetEncryption(r.Context(), tenantID, bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionPutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketEncryption handles GET /{bucket}?encryption. It returns the
// bucket's default-encryption configuration, or 404
// ServerSideEncryptionConfigurationNotFoundError when the bucket has
// none, matching AWS.
func (h *Handler) GetBucketEncryption(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?encryption", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket default encryption is not configured on this gateway", r.URL.Path)
		return
	}
	cfg, err := h.cfg.BucketConfig.GetEncryption(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionGetFailed", err.Error(), r.URL.Path)
		return
	}
	if cfg.Empty() {
		writeError(w, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError", "The server side encryption configuration was not found", r.URL.Path)
		return
	}
	writeXMLDoc(w, sseConfigToXML(cfg))
}

// DeleteBucketEncryption handles DELETE /{bucket}?encryption. It removes
// the bucket's default-encryption configuration and returns 204 No
// Content. Deleting a bucket with no configuration is a no-op success,
// matching AWS's idempotent DeleteBucketEncryption.
func (h *Handler) DeleteBucketEncryption(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "encryption is a bucket-level sub-resource; path must be /{bucket}?encryption", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket default encryption is not configured on this gateway", r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.DeleteEncryption(r.Context(), tenantID, bucket); err != nil {
		writeError(w, http.StatusInternalServerError, "EncryptionDeleteFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- write-path integration ----

// effectiveEncryptionMode layers the bucket default-encryption
// configuration on top of the tenant placement policy to
// produce the encryption mode applied to an object write. It is called
// by Put, CreateMultipartUpload, and Copy after the (destination)
// policy is resolved, so a plaintext source copied into a bucket with a
// default lands encrypted at the destination like a fresh write.
//
// The placement policy is authoritative whenever it already names a
// mode: client_side (Strict ZK) and an explicit managed /
// public_distribution are never overridden by a bucket default — in
// particular no SSE algorithm can ever select client_side, which is
// driven solely by tenant placement. The bucket default only fills the
// gap when the policy leaves the mode empty (plaintext-on-backend): a
// configured default (AES256 or aws:kms, both → managed) promotes the
// write to gateway-side managed encryption, matching S3's "objects are
// encrypted with the bucket default" semantics.
//
// It returns an error only when a default is configured but the gateway
// has no keyring (h.cfg.Encryption == nil). PutBucketEncryption refuses
// to store a default without a keyring, so this is reachable only if the
// keyring was removed after the fact; failing closed (a clear 500 on the
// write) is correct for a security feature rather than silently storing
// plaintext the operator asked to have encrypted.
func (h *Handler) effectiveEncryptionMode(ctx context.Context, tenantID, bucket string, policy metadata.PlacementPolicy) (string, error) {
	if policy.EncryptionMode != "" {
		return policy.EncryptionMode, nil
	}
	if h.cfg.BucketConfig == nil {
		return policy.EncryptionMode, nil
	}
	cfg, err := h.cfg.BucketConfig.GetEncryption(ctx, tenantID, bucket)
	if err != nil {
		return "", err
	}
	if cfg.Empty() {
		return policy.EncryptionMode, nil
	}
	if h.cfg.Encryption == nil {
		return "", errEncryptionNotConfigured
	}
	return string(encryption.ManagedEncrypted), nil
}
