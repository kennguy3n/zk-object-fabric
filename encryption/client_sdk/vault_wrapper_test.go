package client_sdk

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

// fakeVault is a tiny in-memory stand-in for the Transit engine.
// Encrypt b64s the body in a deterministic envelope; Decrypt
// reverses it.
type fakeVault struct {
	mu        sync.Mutex
	encrypts  int
	decrypts  int
	lastToken string
	lastPath  string
	failNext  bool
}

func (f *fakeVault) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/transit/encrypt/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.encrypts++
		f.lastToken = r.Header.Get("X-Vault-Token")
		f.lastPath = r.URL.Path
		if f.failNext {
			f.failNext = false
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": []string{"transit unavailable"}})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		plain, err := base64.StdEncoding.DecodeString(body["plaintext"])
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ciphertext := "vault:v1:" + base64.StdEncoding.EncodeToString(plain)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{"ciphertext": ciphertext},
		})
	})
	mux.HandleFunc("/v1/transit/decrypt/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.decrypts++
		f.lastToken = r.Header.Get("X-Vault-Token")
		f.lastPath = r.URL.Path
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		ct := body["ciphertext"]
		const prefix = "vault:v1:"
		if !strings.HasPrefix(ct, prefix) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": []string{"unrecognized ciphertext"}})
			return
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(ct, prefix))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"plaintext": base64.StdEncoding.EncodeToString(raw),
			},
		})
	})
	return mux
}

func TestVaultWrapper_RoundTrip(t *testing.T) {
	fv := &fakeVault{}
	srv := httptest.NewServer(fv.handler())
	t.Cleanup(srv.Close)

	w := NewVaultWrapper(srv.URL, "test-token", "transit")
	dek := DataEncryptionKey(bytes.Repeat([]byte{0x42}, 32))
	cmk := encryption.CustomerMasterKeyRef{URI: "zkof-prod"}

	wrapped, err := w.WrapDEK(dek, cmk)
	if err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if wrapped.WrapAlgorithm != VaultWrapAlgorithm {
		t.Fatalf("WrapAlgorithm = %q want %q", wrapped.WrapAlgorithm, VaultWrapAlgorithm)
	}
	if !bytes.HasPrefix(wrapped.WrappedKey, []byte("vault:v1:")) {
		t.Fatalf("WrappedKey missing transit prefix: %q", wrapped.WrappedKey)
	}
	if fv.lastPath != "/v1/transit/encrypt/zkof-prod" {
		t.Fatalf("encrypt path = %q", fv.lastPath)
	}
	if fv.lastToken != "test-token" {
		t.Fatalf("token header missing: %q", fv.lastToken)
	}

	got, err := w.UnwrapDEK(wrapped, cmk)
	if err != nil {
		t.Fatalf("UnwrapDEK: %v", err)
	}
	if !bytes.Equal(got, dek) {
		t.Fatalf("unwrapped dek mismatch: got %x want %x", got, dek)
	}
	if fv.decrypts != 1 || fv.encrypts != 1 {
		t.Fatalf("call counts: encrypt=%d decrypt=%d", fv.encrypts, fv.decrypts)
	}
}

func TestVaultWrapper_StripsScheme(t *testing.T) {
	fv := &fakeVault{}
	srv := httptest.NewServer(fv.handler())
	t.Cleanup(srv.Close)

	w := NewVaultWrapper(srv.URL, "tok", "transit")
	if _, err := w.WrapDEK(DataEncryptionKey{1, 2, 3, 4}, encryption.CustomerMasterKeyRef{URI: "vault://k1"}); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if fv.lastPath != "/v1/transit/encrypt/k1" {
		t.Fatalf("encrypt path did not strip scheme: %q", fv.lastPath)
	}
	if _, err := w.WrapDEK(DataEncryptionKey{1, 2, 3, 4}, encryption.CustomerMasterKeyRef{URI: "transit://k2"}); err != nil {
		t.Fatalf("WrapDEK: %v", err)
	}
	if fv.lastPath != "/v1/transit/encrypt/k2" {
		t.Fatalf("encrypt path did not strip transit scheme: %q", fv.lastPath)
	}
}

func TestVaultWrapper_DefaultMount(t *testing.T) {
	fv := &fakeVault{}
	srv := httptest.NewServer(fv.handler())
	t.Cleanup(srv.Close)

	w := NewVaultWrapper(srv.URL, "tok", "")
	if w.mount() != DefaultVaultMount {
		t.Fatalf("default mount = %q", w.mount())
	}
}

func TestVaultWrapper_VaultError(t *testing.T) {
	fv := &fakeVault{failNext: true}
	srv := httptest.NewServer(fv.handler())
	t.Cleanup(srv.Close)

	w := NewVaultWrapper(srv.URL, "tok", "transit")
	_, err := w.WrapDEK(DataEncryptionKey{1, 2, 3}, encryption.CustomerMasterKeyRef{URI: "k"})
	if err == nil {
		t.Fatalf("expected error from vault failure")
	}
	if !strings.Contains(err.Error(), "transit unavailable") {
		t.Fatalf("missing vault error message: %v", err)
	}
}

func TestVaultWrapper_RejectsWrongAlgorithm(t *testing.T) {
	w := NewVaultWrapper("http://x", "t", "")
	_, err := w.UnwrapDEK(WrappedDEK{WrapAlgorithm: "aws-kms-wrap-v1", WrappedKey: []byte("vault:v1:aaa=")},
		encryption.CustomerMasterKeyRef{URI: "k"})
	if err == nil {
		t.Fatalf("expected error for wrong algorithm")
	}
}

func TestVaultWrapper_MissingConfiguration(t *testing.T) {
	if _, err := (&VaultWrapper{}).WrapDEK(DataEncryptionKey{1, 2}, encryption.CustomerMasterKeyRef{URI: "k"}); err == nil {
		t.Fatalf("expected error for missing address")
	}
	if _, err := (&VaultWrapper{Address: "http://x"}).WrapDEK(DataEncryptionKey{1, 2}, encryption.CustomerMasterKeyRef{URI: "k"}); err == nil {
		t.Fatalf("expected error for missing token")
	}
	if _, err := NewVaultWrapper("http://x", "t", "").WrapDEK(DataEncryptionKey{1, 2}, encryption.CustomerMasterKeyRef{URI: ""}); err == nil {
		t.Fatalf("expected error for missing key name")
	}
}
