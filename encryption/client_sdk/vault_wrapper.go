package client_sdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kennguy3n/zk-object-fabric/encryption"
)

// VaultWrapAlgorithm is the canonical wrap algorithm written to
// WrappedDEK.WrapAlgorithm when the gateway envelopes a DEK using
// HashiCorp Vault's Transit secrets engine. The opaque
// "vault:v<n>:<base64>" ciphertext returned by Transit is stored
// verbatim in WrappedKey.
const VaultWrapAlgorithm = "vault-transit-wrap-v1"

// DefaultVaultMount is the conventional mount path for the Transit
// engine. Operators that mount Transit elsewhere supply Mount on
// the wrapper.
const DefaultVaultMount = "transit"

// VaultWrapper seals and opens DEKs via Vault's Transit engine.
// Plaintext keys never leave Vault — the gateway hands Vault a
// 32-byte DEK and stores the returned ciphertext on the manifest.
//
// The wrapper speaks Transit's HTTP API directly with net/http to
// keep the dependency footprint minimal (no vendored vault/api).
type VaultWrapper struct {
	// Address is the Vault server URL, e.g. "https://vault:8200".
	// Required.
	Address string
	// Token is the Vault token used for /encrypt and /decrypt
	// requests. Required. Production typically supplies a
	// short-lived AppRole-issued token via the gateway's
	// environment.
	Token string
	// Mount is the Transit mount path. Defaults to "transit".
	Mount string
	// Namespace is an optional Vault Enterprise namespace passed
	// in the X-Vault-Namespace header.
	Namespace string
	// HTTPClient overrides the default HTTP client. Useful for
	// tests and for plugging in a custom TLS config.
	HTTPClient *http.Client
	// Context is the context applied to outbound requests.
	// Defaults to context.Background() with a 30s timeout.
	Context context.Context
}

// NewVaultWrapper builds a wrapper bound to address and token.
// mount may be empty to use DefaultVaultMount.
func NewVaultWrapper(address, token, mount string) *VaultWrapper {
	if mount == "" {
		mount = DefaultVaultMount
	}
	return &VaultWrapper{Address: address, Token: token, Mount: mount}
}

func (w *VaultWrapper) httpClient() *http.Client {
	if w.HTTPClient != nil {
		return w.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (w *VaultWrapper) ctx() context.Context {
	if w.Context != nil {
		return w.Context
	}
	return context.Background()
}

func (w *VaultWrapper) mount() string {
	if w.Mount == "" {
		return DefaultVaultMount
	}
	return w.Mount
}

// WrapDEK seals dek with the Transit key named by cmk.URI. The
// URI may be either a plain Transit key name ("zkof-prod") or a
// vault://-scheme URI ("vault://zkof-prod"); transit:// is also
// accepted as an alias.
func (w *VaultWrapper) WrapDEK(dek DataEncryptionKey, cmk encryption.CustomerMasterKeyRef) (WrappedDEK, error) {
	if w == nil {
		return WrappedDEK{}, errors.New("client_sdk: nil VaultWrapper")
	}
	if w.Address == "" {
		return WrappedDEK{}, errors.New("client_sdk: VaultWrapper.Address is required")
	}
	if w.Token == "" {
		return WrappedDEK{}, errors.New("client_sdk: VaultWrapper.Token is required")
	}
	if len(dek) == 0 {
		return WrappedDEK{}, errors.New("client_sdk: VaultWrapper: empty dek")
	}
	keyName := normalizeVaultKeyName(cmk.URI)
	if keyName == "" {
		return WrappedDEK{}, errors.New("client_sdk: VaultWrapper requires cmk.URI (transit key name)")
	}
	body := map[string]string{
		"plaintext": base64.StdEncoding.EncodeToString(dek),
	}
	resp, err := w.do(w.ctx(), http.MethodPost, "/v1/"+w.mount()+"/encrypt/"+url.PathEscape(keyName), body)
	if err != nil {
		return WrappedDEK{}, fmt.Errorf("client_sdk: vault encrypt: %w", err)
	}
	ciphertext, _ := resp.Data["ciphertext"].(string)
	if ciphertext == "" {
		return WrappedDEK{}, errors.New("client_sdk: vault encrypt: missing ciphertext")
	}
	return WrappedDEK{
		KeyID:         dekKeyID(dek, cmk),
		Algorithm:     ContentAlgorithm,
		WrappedKey:    []byte(ciphertext),
		WrapAlgorithm: VaultWrapAlgorithm,
	}, nil
}

// UnwrapDEK round-trips the stored Transit ciphertext through
// /decrypt/{key_name} and returns the recovered plaintext DEK.
func (w *VaultWrapper) UnwrapDEK(wrapped WrappedDEK, cmk encryption.CustomerMasterKeyRef) (DataEncryptionKey, error) {
	if w == nil {
		return nil, errors.New("client_sdk: nil VaultWrapper")
	}
	if wrapped.WrapAlgorithm != VaultWrapAlgorithm {
		return nil, fmt.Errorf("client_sdk: VaultWrapper: unexpected wrap algorithm %q", wrapped.WrapAlgorithm)
	}
	if w.Address == "" || w.Token == "" {
		return nil, errors.New("client_sdk: VaultWrapper requires Address and Token")
	}
	keyName := normalizeVaultKeyName(cmk.URI)
	if keyName == "" {
		return nil, errors.New("client_sdk: VaultWrapper requires cmk.URI")
	}
	body := map[string]string{"ciphertext": string(wrapped.WrappedKey)}
	resp, err := w.do(w.ctx(), http.MethodPost, "/v1/"+w.mount()+"/decrypt/"+url.PathEscape(keyName), body)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: vault decrypt: %w", err)
	}
	plain, _ := resp.Data["plaintext"].(string)
	if plain == "" {
		return nil, errors.New("client_sdk: vault decrypt: missing plaintext")
	}
	dek, err := base64.StdEncoding.DecodeString(plain)
	if err != nil {
		return nil, fmt.Errorf("client_sdk: vault decrypt: decode plaintext: %w", err)
	}
	return DataEncryptionKey(dek), nil
}

// vaultResponse is the subset of the Vault API response shape the
// wrapper consumes.
type vaultResponse struct {
	Data   map[string]interface{} `json:"data"`
	Errors []string               `json:"errors"`
}

// do issues a Vault HTTP request and decodes the JSON envelope.
// Non-2xx responses are surfaced as errors that include the Vault
// error list when present.
func (w *VaultWrapper) do(ctx context.Context, method, path string, payload interface{}) (*vaultResponse, error) {
	url := strings.TrimRight(w.Address, "/") + path
	var bodyReader io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("encode body: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("X-Vault-Token", w.Token)
	if w.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", w.Namespace)
	}
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	var vr vaultResponse
	if len(body) > 0 {
		// Vault returns 2xx with a JSON body on success and a
		// JSON body with {"errors":[…]} on most failures. A
		// non-JSON body usually indicates a proxy error; surface
		// the raw text so operators can grep for it.
		if jerr := json.Unmarshal(body, &vr); jerr != nil {
			if resp.StatusCode/100 == 2 {
				return nil, fmt.Errorf("decode body: %w", jerr)
			}
			return nil, fmt.Errorf("vault status %d: %s", resp.StatusCode, string(body))
		}
	}
	if resp.StatusCode/100 != 2 {
		if len(vr.Errors) > 0 {
			return nil, fmt.Errorf("vault status %d: %s", resp.StatusCode, strings.Join(vr.Errors, "; "))
		}
		return nil, fmt.Errorf("vault status %d", resp.StatusCode)
	}
	return &vr, nil
}

// normalizeVaultKeyName strips known scheme prefixes ("vault://",
// "transit://") from a CMK URI so callers can use either a
// vendor-neutral URI or a bare Transit key name.
func normalizeVaultKeyName(uri string) string {
	for _, prefix := range []string{"vault://", "transit://"} {
		if strings.HasPrefix(uri, prefix) {
			return strings.TrimPrefix(uri, prefix)
		}
	}
	return uri
}
