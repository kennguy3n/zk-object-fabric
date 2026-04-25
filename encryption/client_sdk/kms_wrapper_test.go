package client_sdk

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

// fakeKMS is an in-memory KMSAPI used to exercise KMSWrapper
// without making a real KMS call. Encrypt prepends the keyId so
// Decrypt can recover both the plaintext and the bound key.
type fakeKMS struct {
	encryptCalls int
	decryptCalls int
	lastKeyID    string
	overrideKey  string
	encryptErr   error
	decryptErr   error
}

const fakeMagic = "kmsfake|"

func (f *fakeKMS) Encrypt(ctx context.Context, in *kms.EncryptInput, _ ...func(*kms.Options)) (*kms.EncryptOutput, error) {
	f.encryptCalls++
	if f.encryptErr != nil {
		return nil, f.encryptErr
	}
	keyID := ""
	if in.KeyId != nil {
		keyID = *in.KeyId
	}
	f.lastKeyID = keyID
	blob := []byte(fakeMagic + keyID + "|")
	blob = append(blob, in.Plaintext...)
	return &kms.EncryptOutput{
		CiphertextBlob: blob,
		KeyId:          aws.String(keyID),
	}, nil
}

func (f *fakeKMS) Decrypt(ctx context.Context, in *kms.DecryptInput, _ ...func(*kms.Options)) (*kms.DecryptOutput, error) {
	f.decryptCalls++
	if f.decryptErr != nil {
		return nil, f.decryptErr
	}
	if !bytes.HasPrefix(in.CiphertextBlob, []byte(fakeMagic)) {
		return nil, errors.New("fakeKMS: unrecognized ciphertext")
	}
	rest := in.CiphertextBlob[len(fakeMagic):]
	sep := bytes.IndexByte(rest, '|')
	if sep < 0 {
		return nil, errors.New("fakeKMS: malformed ciphertext")
	}
	keyID := string(rest[:sep])
	plaintext := rest[sep+1:]
	out := &kms.DecryptOutput{Plaintext: plaintext}
	if f.overrideKey != "" {
		out.KeyId = aws.String(f.overrideKey)
	} else {
		out.KeyId = aws.String(keyID)
	}
	return out, nil
}

func TestKMSWrapper_RoundTrip(t *testing.T) {
	fake := &fakeKMS{}
	w := NewKMSWrapper(fake)
	dek := DataEncryptionKey(bytes.Repeat([]byte{0xab}, 32))
	cmk := encryption.CustomerMasterKeyRef{
		URI:         "arn:aws:kms:us-east-1:123456789012:key/abcd-1234",
		Version:     1,
		HolderClass: "aws_kms",
	}

	wrapped, err := w.WrapDEK(dek, cmk)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if wrapped.WrapAlgorithm != KMSWrapAlgorithm {
		t.Fatalf("WrapAlgorithm = %q want %q", wrapped.WrapAlgorithm, KMSWrapAlgorithm)
	}
	if len(wrapped.WrappedKey) == 0 {
		t.Fatalf("WrappedKey is empty")
	}
	if fake.lastKeyID != cmk.URI {
		t.Fatalf("lastKeyID = %q want %q", fake.lastKeyID, cmk.URI)
	}

	got, err := w.UnwrapDEK(wrapped, cmk)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("unwrapped dek mismatch: got %x want %x", got, dek)
	}
	if fake.encryptCalls != 1 || fake.decryptCalls != 1 {
		t.Fatalf("call counts: encrypt=%d decrypt=%d", fake.encryptCalls, fake.decryptCalls)
	}
}

func TestKMSWrapper_StripsKMSScheme(t *testing.T) {
	fake := &fakeKMS{}
	w := NewKMSWrapper(fake)
	dek := DataEncryptionKey(bytes.Repeat([]byte{0x01}, 32))
	cmk := encryption.CustomerMasterKeyRef{URI: "kms://alias/zkof-prod"}

	if _, err := w.WrapDEK(dek, cmk); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if fake.lastKeyID != "alias/zkof-prod" {
		t.Fatalf("KMS scheme not stripped; lastKeyID = %q", fake.lastKeyID)
	}
}

func TestKMSWrapper_WrongAlgorithm(t *testing.T) {
	w := NewKMSWrapper(&fakeKMS{})
	_, err := w.UnwrapDEK(WrappedDEK{WrapAlgorithm: "vault-transit-wrap-v1", WrappedKey: []byte("x")},
		encryption.CustomerMasterKeyRef{URI: "arn:aws:kms:us-east-1:1:key/abc"})
	if err == nil {
		t.Fatalf("expected error for wrong wrap algorithm")
	}
}

func TestKMSWrapper_KeyIDMismatch(t *testing.T) {
	fake := &fakeKMS{}
	w := NewKMSWrapper(fake)
	dek := DataEncryptionKey(bytes.Repeat([]byte{0x02}, 32))
	cmk := encryption.CustomerMasterKeyRef{URI: "arn:aws:kms:us-east-1:1:key/abc"}
	wrapped, err := w.WrapDEK(dek, cmk)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	// Force the fake to report a different KeyId on Decrypt to
	// simulate a tampered envelope opened by a different CMK.
	fake.overrideKey = "arn:aws:kms:us-east-1:1:key/XYZ"
	if _, err := w.UnwrapDEK(wrapped, cmk); err == nil {
		t.Fatalf("expected error on KeyId mismatch")
	}
}

func TestKMSWrapper_KeyIDARNAcceptsAlias(t *testing.T) {
	fake := &fakeKMS{}
	w := NewKMSWrapper(fake)
	dek := DataEncryptionKey(bytes.Repeat([]byte{0x03}, 32))
	// Configure CMK with the bare alias; KMS canonicalizes the
	// returned KeyId to the full ARN. The wrapper must accept
	// the suffix-matched response.
	cmk := encryption.CustomerMasterKeyRef{URI: "alias/zkof-prod"}
	wrapped, err := w.WrapDEK(dek, cmk)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	fake.overrideKey = "arn:aws:kms:us-east-1:1:alias/zkof-prod"
	got, err := w.UnwrapDEK(wrapped, cmk)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("unwrapped dek mismatch")
	}
}

func TestKMSWrapper_NilClient(t *testing.T) {
	var w *KMSWrapper
	if _, err := w.WrapDEK(DataEncryptionKey{1, 2, 3}, encryption.CustomerMasterKeyRef{URI: "k"}); err == nil {
		t.Fatalf("expected error on nil client")
	}
}

func TestKMSWrapper_EncryptError(t *testing.T) {
	fake := &fakeKMS{encryptErr: errors.New("kms unavailable")}
	w := NewKMSWrapper(fake)
	if _, err := w.WrapDEK(DataEncryptionKey{1, 2, 3}, encryption.CustomerMasterKeyRef{URI: "k"}); err == nil {
		t.Fatalf("expected encrypt error to propagate")
	}
}
