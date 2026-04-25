package client_sdk

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

// KMSWrapAlgorithm is the canonical wrap algorithm written to
// WrappedDEK.WrapAlgorithm when the gateway envelopes a DEK using
// AWS KMS. The opaque KMS ciphertext blob is stored verbatim in
// WrappedKey; UnwrapDEK round-trips the blob back through KMS to
// recover the plaintext DEK.
const KMSWrapAlgorithm = "aws-kms-wrap-v1"

// KMSAPI is the subset of the AWS KMS client that KMSWrapper uses.
// Tests supply a fake implementation; production wires the real
// kms.Client constructed via kms.NewFromConfig.
type KMSAPI interface {
	Encrypt(ctx context.Context, params *kms.EncryptInput, optFns ...func(*kms.Options)) (*kms.EncryptOutput, error)
	Decrypt(ctx context.Context, params *kms.DecryptInput, optFns ...func(*kms.Options)) (*kms.DecryptOutput, error)
}

// KMSWrapper seals and opens DEKs using AWS KMS as the customer
// master key holder. The plaintext CMK never leaves KMS — the
// gateway hands KMS a 32-byte DEK and stores the returned
// ciphertext blob on the manifest. This replaces LocalFileWrapper
// for production deployments where keeping the master key on disk
// is not acceptable.
type KMSWrapper struct {
	// Client is the configured KMS client. Required.
	Client KMSAPI
	// Context is the context used for KMS calls. Optional;
	// defaults to context.Background.
	Context context.Context
}

// NewKMSWrapper constructs a wrapper bound to client. The caller is
// responsible for building the KMS client (e.g. via
// kms.NewFromConfig with the desired region and credential
// provider).
func NewKMSWrapper(client KMSAPI) *KMSWrapper {
	if client == nil {
		return nil
	}
	return &KMSWrapper{Client: client}
}

func (w *KMSWrapper) ctx() context.Context {
	if w.Context != nil {
		return w.Context
	}
	return context.Background()
}

// WrapDEK seals dek with the KMS key identified by cmk.URI. The
// URI is passed verbatim to KMS as KeyId, so callers may use a
// KMS key ARN ("arn:aws:kms:..."), a key ID, or an alias
// ("alias/...") — KMS resolves all three.
func (w *KMSWrapper) WrapDEK(dek DataEncryptionKey, cmk encryption.CustomerMasterKeyRef) (WrappedDEK, error) {
	if w == nil || w.Client == nil {
		return WrappedDEK{}, errors.New("client_sdk: KMSWrapper requires a non-nil KMS client")
	}
	if cmk.URI == "" {
		return WrappedDEK{}, errors.New("client_sdk: KMSWrapper requires cmk.URI (KMS KeyId / ARN / alias)")
	}
	if len(dek) == 0 {
		return WrappedDEK{}, errors.New("client_sdk: KMSWrapper: empty dek")
	}
	keyID := normalizeKMSKeyID(cmk.URI)
	out, err := w.Client.Encrypt(w.ctx(), &kms.EncryptInput{
		KeyId:     aws.String(keyID),
		Plaintext: append([]byte(nil), dek...),
	})
	if err != nil {
		return WrappedDEK{}, fmt.Errorf("client_sdk: kms encrypt: %w", err)
	}
	if out == nil || len(out.CiphertextBlob) == 0 {
		return WrappedDEK{}, errors.New("client_sdk: kms encrypt returned empty ciphertext")
	}
	return WrappedDEK{
		KeyID:         dekKeyID(dek, cmk),
		Algorithm:     ContentAlgorithm,
		WrappedKey:    out.CiphertextBlob,
		WrapAlgorithm: KMSWrapAlgorithm,
	}, nil
}

// UnwrapDEK round-trips the stored ciphertext blob through
// kms.Decrypt and returns the plaintext DEK. The returned KMS
// KeyId is checked against cmk.URI so a manifest written under a
// different CMK cannot be opened by accident or by a tampered
// envelope.
func (w *KMSWrapper) UnwrapDEK(wrapped WrappedDEK, cmk encryption.CustomerMasterKeyRef) (DataEncryptionKey, error) {
	if w == nil || w.Client == nil {
		return nil, errors.New("client_sdk: KMSWrapper requires a non-nil KMS client")
	}
	if wrapped.WrapAlgorithm != KMSWrapAlgorithm {
		return nil, fmt.Errorf("client_sdk: KMSWrapper: unexpected wrap algorithm %q", wrapped.WrapAlgorithm)
	}
	if len(wrapped.WrappedKey) == 0 {
		return nil, errors.New("client_sdk: KMSWrapper: empty wrapped key")
	}
	expectedKey := normalizeKMSKeyID(cmk.URI)
	in := &kms.DecryptInput{CiphertextBlob: wrapped.WrappedKey}
	if expectedKey != "" {
		in.KeyId = aws.String(expectedKey)
	}
	out, err := w.Client.Decrypt(w.ctx(), in)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: kms decrypt: %w", err)
	}
	if out == nil || len(out.Plaintext) == 0 {
		return nil, errors.New("client_sdk: kms decrypt returned empty plaintext")
	}
	if expectedKey != "" && out.KeyId != nil && !kmsKeyIDsEqual(*out.KeyId, expectedKey) {
		return nil, fmt.Errorf("client_sdk: kms decrypt key mismatch: got %q want %q", *out.KeyId, expectedKey)
	}
	return DataEncryptionKey(out.Plaintext), nil
}

// normalizeKMSKeyID strips known scheme prefixes ("kms://") so
// callers can use either a vendor-neutral URI or a raw KMS KeyId /
// ARN / alias. Other prefixes (notably "arn:aws:kms:...") pass
// through unchanged because KMS accepts them as KeyId directly.
func normalizeKMSKeyID(uri string) string {
	if len(uri) > len("kms://") && uri[:len("kms://")] == "kms://" {
		return uri[len("kms://"):]
	}
	return uri
}

// kmsKeyIDsEqual compares two KMS KeyId strings for equality using
// a constant-time check. KMS returns the canonical ARN even when
// the request used an alias, so we fall back to a suffix match
// when the strings are not identical.
func kmsKeyIDsEqual(a, b string) bool {
	if subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 {
		return true
	}
	// KMS canonicalizes alias/ID inputs to a full ARN. Treat a
	// suffix match as equal so callers configured with the bare
	// KeyId or alias still pass verification.
	if hasSuffix(a, b) || hasSuffix(b, a) {
		return true
	}
	return false
}

func hasSuffix(s, suffix string) bool {
	if len(suffix) == 0 || len(suffix) > len(s) {
		return false
	}
	return s[len(s)-len(suffix):] == suffix
}
