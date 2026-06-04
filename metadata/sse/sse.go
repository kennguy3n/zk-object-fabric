// Package sse defines the provider-neutral domain types for S3 bucket
// default server-side encryption (SSE) configuration (WS8.7): the
// per-bucket default that PutBucketEncryption sets and
// GetBucketEncryption returns.
//
// It carries no persistence of its own — bucket-level Config is stored
// through metadata/bucket_config (the per-bucket S3 config store,
// shared with versioning, Object Lock, CORS, and lifecycle). The
// api/s3compat layer maps between this type and the
// <ServerSideEncryptionConfiguration> XML document and translates the
// SSE algorithm into the ZKOF encryption mode applied to writes; this
// package only owns the value semantics (a valid algorithm, the
// optional KMS key, and the bucket-key flag).
//
// AWS exposes two bucket-default SSE algorithms: "AES256" (SSE-S3,
// service-managed keys) and "aws:kms" (SSE-KMS, a named CMK). ZKOF has
// no separate SSE-S3 vs SSE-KMS key hierarchy — both select the
// gateway-side ManagedEncrypted mode (the gateway holds the
// DEK-wrapping key and encrypts on the tenant's behalf). The mapping
// from algorithm to ZKOF mode lives in api/s3compat, keeping this
// package free of any dependency on the encryption layer. There is no
// SSE algorithm that selects client_side (Strict ZK): that mode is
// driven by tenant placement policy, never by a bucket default.
package sse

import (
	"errors"
	"fmt"
)

// Algorithm is the SSEAlgorithm value from an S3
// <ServerSideEncryptionByDefault> element.
type Algorithm string

const (
	// AES256 is SSE-S3 in AWS terms (service-managed keys). In ZKOF it
	// maps to gateway-side ManagedEncrypted.
	AES256 Algorithm = "AES256"
	// AWSKMS is SSE-KMS in AWS terms (a named CMK). In ZKOF it also maps
	// to gateway-side ManagedEncrypted; KMSMasterKeyID, when set, names
	// the customer master key used to wrap per-object DEKs.
	AWSKMS Algorithm = "aws:kms"
)

// Config is the bucket-level default SSE configuration set by
// PutBucketEncryption. A zero Config (empty Algorithm) means the bucket
// has no default-encryption configuration, which GetBucketEncryption
// surfaces as the 404 ServerSideEncryptionConfigurationNotFoundError.
//
// S3's ServerSideEncryptionConfiguration carries a list of Rules, but
// the schema permits exactly one rule, so the domain type flattens it
// to a single default. PutBucketEncryption rejects documents with more
// than one rule.
type Config struct {
	// Algorithm is the default SSEAlgorithm ("AES256" or "aws:kms").
	Algorithm Algorithm
	// KMSMasterKeyID is the KMS key id/ARN from
	// <KMSMasterKeyID>. Only valid with the aws:kms algorithm; optional
	// even then (AWS falls back to the account default KMS key).
	KMSMasterKeyID string
	// BucketKeyEnabled mirrors <BucketKeyEnabled>. It is recorded for
	// round-trip fidelity with the AWS API; ZKOF derives per-object DEKs
	// regardless, so the flag has no effect on stored ciphertext.
	BucketKeyEnabled bool
}

// Empty reports whether the bucket has no default SSE configured.
func (c Config) Empty() bool {
	return c.Algorithm == ""
}

// Valid checks that the configuration is well-formed per the S3
// PutBucketEncryption contract. It is called by PutBucketEncryption
// before persisting. An empty Config is not valid: S3 rejects a
// PutBucketEncryption with no rule, and a cleared default is expressed
// through DeleteBucketEncryption, not by storing an empty Config.
func (c Config) Valid() error {
	switch c.Algorithm {
	case AES256:
		if c.KMSMasterKeyID != "" {
			return errors.New("sse: KMSMasterKeyID is only valid with the aws:kms algorithm")
		}
	case AWSKMS:
		// KMSMasterKeyID is optional (account-default KMS key when unset).
	case "":
		return errors.New("sse: SSEAlgorithm is required")
	default:
		return fmt.Errorf("sse: SSEAlgorithm %q must be one of AES256 or aws:kms", c.Algorithm)
	}
	return nil
}
