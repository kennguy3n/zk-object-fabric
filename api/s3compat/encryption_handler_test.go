package s3compat

import (
	"context"
	"crypto/rand"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/metadata/sse"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// newEncryptionTestHandler builds a handler wired with both a
// BucketConfig store (so the ?encryption sub-resource is supported) and
// a gateway keyring (so a bucket default can be honored). It mirrors
// newVersioningTestHandler's store + advancing-clock setup and
// newAADTestHandler's LocalFileWrapper keyring, the two halves WS8.7
// exercises together.
func newEncryptionTestHandler(t *testing.T) (*Handler, bucket_config.Store, manifest_store.ManifestStore) {
	t.Helper()
	store := memory.New()
	cfg := bucket_config.NewMemoryStore()
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:    store,
		Providers:    map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:    fixedPlacement{backend: "test"},
		Billing:      &recordingBilling{},
		BucketConfig: cfg,
		Encryption:   newTestKeyring(t),
		Now: func() time.Time {
			t := now
			now = now.Add(time.Second)
			return t
		},
	})
	return h, cfg, store
}

// newTestKeyring builds a GatewayEncryption backed by a fresh CMK on
// disk, matching newAADTestHandler.
func newTestKeyring(t *testing.T) *GatewayEncryption {
	t.Helper()
	cmkPath := filepath.Join(t.TempDir(), "cmk.key")
	cmk := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(cmk); err != nil {
		t.Fatalf("rand cmk: %v", err)
	}
	if err := os.WriteFile(cmkPath, cmk, 0o600); err != nil {
		t.Fatalf("write cmk: %v", err)
	}
	return &GatewayEncryption{
		Wrapper: client_sdk.LocalFileWrapper{Path: cmkPath},
		CMK: encryption.CustomerMasterKeyRef{
			URI:         "cmk://test/primary",
			Version:     1,
			HolderClass: "gateway_hsm",
		},
	}
}

func putEncryption(t *testing.T, h *Handler, bucket, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?encryption", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	return rec
}

const aes256EncryptionBody = `<ServerSideEncryptionConfiguration><Rule>` +
	`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault>` +
	`</Rule></ServerSideEncryptionConfiguration>`

func TestPutGetDeleteBucketEncryption_RoundTrip(t *testing.T) {
	h, _, _ := newEncryptionTestHandler(t)

	// Unconfigured bucket: GET is 404 ServerSideEncryptionConfigurationNotFoundError.
	req := httptest.NewRequest(http.MethodGet, "/bucket?encryption", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?encryption unconfigured = %d, want 404; body=%s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, "ServerSideEncryptionConfigurationNotFoundError") {
		t.Errorf("unconfigured GET body = %s, want ServerSideEncryptionConfigurationNotFoundError", body)
	}

	// PUT a valid AES256 default.
	if rec := putEncryption(t, h, "bucket", aes256EncryptionBody); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?encryption AES256 = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// GET reads it back.
	req = httptest.NewRequest(http.MethodGet, "/bucket?encryption", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?encryption configured = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc serverSideEncryptionConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Rules) != 1 {
		t.Fatalf("Rules len = %d, want 1", len(doc.Rules))
	}
	if got := doc.Rules[0].ApplyByDefault.SSEAlgorithm; got != "AES256" {
		t.Errorf("SSEAlgorithm = %q, want AES256", got)
	}
	if got := doc.Rules[0].ApplyByDefault.KMSMasterKeyID; got != "" {
		t.Errorf("KMSMasterKeyID = %q, want empty for AES256", got)
	}

	// DELETE clears it (204).
	req = httptest.NewRequest(http.MethodDelete, "/bucket?encryption", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?encryption = %d, want 204; body=%s", rec.Code, rec.Body)
	}

	// GET after DELETE is 404 again.
	req = httptest.NewRequest(http.MethodGet, "/bucket?encryption", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?encryption after delete = %d, want 404; body=%s", rec.Code, rec.Body)
	}

	// DELETE again is idempotent (still 204, matching AWS).
	req = httptest.NewRequest(http.MethodDelete, "/bucket?encryption", nil)
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?encryption (idempotent) = %d, want 204; body=%s", rec.Code, rec.Body)
	}
}

func TestPutBucketEncryption_KMSRoundTrip(t *testing.T) {
	h, _, _ := newEncryptionTestHandler(t)
	body := `<ServerSideEncryptionConfiguration><Rule>` +
		`<ApplyServerSideEncryptionByDefault>` +
		`<SSEAlgorithm>aws:kms</SSEAlgorithm>` +
		`<KMSMasterKeyID>arn:aws:kms:us-east-1:111122223333:key/abc</KMSMasterKeyID>` +
		`</ApplyServerSideEncryptionByDefault>` +
		`<BucketKeyEnabled>true</BucketKeyEnabled>` +
		`</Rule></ServerSideEncryptionConfiguration>`
	if rec := putEncryption(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?encryption aws:kms = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket?encryption", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?encryption = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc serverSideEncryptionConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rule := doc.Rules[0]
	if rule.ApplyByDefault.SSEAlgorithm != "aws:kms" {
		t.Errorf("SSEAlgorithm = %q, want aws:kms", rule.ApplyByDefault.SSEAlgorithm)
	}
	if rule.ApplyByDefault.KMSMasterKeyID != "arn:aws:kms:us-east-1:111122223333:key/abc" {
		t.Errorf("KMSMasterKeyID = %q, want the ARN round-tripped", rule.ApplyByDefault.KMSMasterKeyID)
	}
	if !rule.BucketKeyEnabled {
		t.Error("BucketKeyEnabled = false, want true round-tripped")
	}
}

func TestPutBucketEncryption_Validation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{
			name:     "malformed xml",
			body:     "<ServerSideEncryptionConfiguration><Rule>",
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
		{
			name:     "zero rules",
			body:     "<ServerSideEncryptionConfiguration></ServerSideEncryptionConfiguration>",
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
		{
			name: "two rules",
			body: `<ServerSideEncryptionConfiguration>` +
				`<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule>` +
				`<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule>` +
				`</ServerSideEncryptionConfiguration>`,
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
		{
			name: "aes256 with kms key id",
			body: `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault>` +
				`<SSEAlgorithm>AES256</SSEAlgorithm><KMSMasterKeyID>arn:key</KMSMasterKeyID>` +
				`</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`,
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
		{
			name: "unknown algorithm",
			body: `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault>` +
				`<SSEAlgorithm>rot13</SSEAlgorithm></ApplyServerSideEncryptionByDefault></Rule>` +
				`</ServerSideEncryptionConfiguration>`,
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
		{
			name: "missing algorithm",
			body: `<ServerSideEncryptionConfiguration><Rule><ApplyServerSideEncryptionByDefault>` +
				`</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`,
			wantCode: http.StatusBadRequest,
			wantErr:  "MalformedXML",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newEncryptionTestHandler(t)
			rec := putEncryption(t, h, "bucket", tc.body)
			if rec.Code != tc.wantCode {
				t.Fatalf("PUT %s = %d, want %d; body=%s", tc.name, rec.Code, tc.wantCode, rec.Body)
			}
			if !strings.Contains(rec.Body.String(), tc.wantErr) {
				t.Errorf("PUT %s body = %s, want code %s", tc.name, rec.Body, tc.wantErr)
			}
		})
	}
}

// TestPutBucketEncryption_NoKeyring asserts the fail-closed guard:
// without a gateway keyring the bucket default cannot be honored, so
// PutBucketEncryption refuses the configuration up front (501) rather
// than storing a default that would 500 every subsequent object PUT.
func TestPutBucketEncryption_NoKeyring(t *testing.T) {
	h := New(Config{
		Manifests:    memory.New(),
		Providers:    map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:    fixedPlacement{backend: "test"},
		Billing:      &recordingBilling{},
		BucketConfig: bucket_config.NewMemoryStore(),
		// Encryption deliberately nil.
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	})
	rec := putEncryption(t, h, "bucket", aes256EncryptionBody)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("PUT ?encryption without keyring = %d, want 501; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "gateway-side encryption is not configured") {
		t.Errorf("body = %s, want keyring-not-configured guidance", rec.Body)
	}
}

// TestBucketEncryption_NoStore asserts that without a BucketConfig store
// all three verbs report 501 NotImplemented rather than panicking.
func TestBucketEncryption_NoStore(t *testing.T) {
	h := New(Config{
		Manifests:  memory.New(),
		Providers:  map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:  fixedPlacement{backend: "test"},
		Billing:    &recordingBilling{},
		Encryption: newTestKeyring(t),
		Now:        func() time.Time { return time.Unix(1700000000, 0) },
	})
	for _, m := range []string{http.MethodPut, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(m, "/bucket?encryption", strings.NewReader(aes256EncryptionBody))
		rec := httptest.NewRecorder()
		h.dispatch(rec, req)
		if rec.Code != http.StatusNotImplemented {
			t.Errorf("%s ?encryption without store = %d, want 501; body=%s", m, rec.Code, rec.Body)
		}
	}
}

// TestEffectiveEncryptionMode unit-tests the write-path layering: the
// placement policy is authoritative when it names a mode, and a bucket
// default only fills an empty mode (promoting to managed).
func TestEffectiveEncryptionMode(t *testing.T) {
	ctx := context.Background()

	setDefault := func(t *testing.T, store bucket_config.Store, alg sse.Algorithm) {
		t.Helper()
		if err := store.SetEncryption(ctx, AnonymousTenant, "bucket", sse.Config{Algorithm: alg}); err != nil {
			t.Fatalf("SetEncryption: %v", err)
		}
	}

	t.Run("policy mode is authoritative over bucket default", func(t *testing.T) {
		h, store, _ := newEncryptionTestHandler(t)
		setDefault(t, store, sse.AES256)
		// client_side (Strict ZK) must never be overridden by a default.
		policy := metadata.PlacementPolicy{EncryptionMode: string(encryption.StrictZK)}
		got, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", policy)
		if err != nil {
			t.Fatalf("effectiveEncryptionMode: %v", err)
		}
		if got != string(encryption.StrictZK) {
			t.Errorf("mode = %q, want client_side preserved", got)
		}
	})

	t.Run("empty mode no default stays empty", func(t *testing.T) {
		h, _, _ := newEncryptionTestHandler(t)
		got, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", metadata.PlacementPolicy{})
		if err != nil {
			t.Fatalf("effectiveEncryptionMode: %v", err)
		}
		if got != "" {
			t.Errorf("mode = %q, want empty (no default configured)", got)
		}
	})

	t.Run("AES256 default promotes empty to managed", func(t *testing.T) {
		h, store, _ := newEncryptionTestHandler(t)
		setDefault(t, store, sse.AES256)
		got, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", metadata.PlacementPolicy{})
		if err != nil {
			t.Fatalf("effectiveEncryptionMode: %v", err)
		}
		if got != string(encryption.ManagedEncrypted) {
			t.Errorf("mode = %q, want managed", got)
		}
	})

	t.Run("aws:kms default promotes empty to managed", func(t *testing.T) {
		h, store, _ := newEncryptionTestHandler(t)
		setDefault(t, store, sse.AWSKMS)
		got, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", metadata.PlacementPolicy{})
		if err != nil {
			t.Fatalf("effectiveEncryptionMode: %v", err)
		}
		if got != string(encryption.ManagedEncrypted) {
			t.Errorf("mode = %q, want managed", got)
		}
	})

	t.Run("no BucketConfig store passes policy through", func(t *testing.T) {
		h := New(Config{
			Manifests:  memory.New(),
			Providers:  map[string]providers.StorageProvider{"test": newFakeProvider("test")},
			Placement:  fixedPlacement{backend: "test"},
			Billing:    &recordingBilling{},
			Encryption: newTestKeyring(t),
			Now:        func() time.Time { return time.Unix(1700000000, 0) },
		})
		got, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", metadata.PlacementPolicy{})
		if err != nil {
			t.Fatalf("effectiveEncryptionMode: %v", err)
		}
		if got != "" {
			t.Errorf("mode = %q, want empty passthrough", got)
		}
	})

	t.Run("default configured but keyring removed fails closed", func(t *testing.T) {
		store := bucket_config.NewMemoryStore()
		if err := store.SetEncryption(ctx, AnonymousTenant, "bucket", sse.Config{Algorithm: sse.AES256}); err != nil {
			t.Fatalf("SetEncryption: %v", err)
		}
		// Handler with a default in the store but no keyring: simulates a
		// keyring removed after PutBucketEncryption stored the default.
		h := New(Config{
			Manifests:    memory.New(),
			Providers:    map[string]providers.StorageProvider{"test": newFakeProvider("test")},
			Placement:    fixedPlacement{backend: "test"},
			Billing:      &recordingBilling{},
			BucketConfig: store,
			// Encryption nil.
			Now: func() time.Time { return time.Unix(1700000000, 0) },
		})
		_, err := h.effectiveEncryptionMode(ctx, AnonymousTenant, "bucket", metadata.PlacementPolicy{})
		if err == nil {
			t.Fatal("effectiveEncryptionMode = nil error, want fail-closed error")
		}
	})
}

// TestPutObject_BucketDefaultPromotesToManaged is the end-to-end Put
// wiring check: with an AES256 bucket default configured, an object
// written without an explicit mode is stored gateway-managed-encrypted;
// without a default it stays unencrypted (empty mode).
func TestPutObject_BucketDefaultPromotesToManaged(t *testing.T) {
	h, _, store := newEncryptionTestHandler(t)

	// Control: no default → object PUT records an empty (legacy) mode.
	body := "plaintext-default"
	req := httptest.NewRequest(http.MethodPut, "/bucket/plain", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT plain object = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	man, err := store.Get(context.Background(), manifest_store.ManifestKey{
		TenantID: AnonymousTenant, Bucket: "bucket", ObjectKeyHash: hashObjectKey("plain"),
	})
	if err != nil {
		t.Fatalf("manifest get (plain): %v", err)
	}
	if man.Encryption.Mode != "" {
		t.Errorf("plain object Mode = %q, want empty (no default)", man.Encryption.Mode)
	}

	// Configure an AES256 default, then PUT a fresh object.
	if rec := putEncryption(t, h, "bucket", aes256EncryptionBody); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?encryption = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	body = "should-be-managed"
	req = httptest.NewRequest(http.MethodPut, "/bucket/enc", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	rec = httptest.NewRecorder()
	h.dispatch(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT object under default = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	man, err = store.Get(context.Background(), manifest_store.ManifestKey{
		TenantID: AnonymousTenant, Bucket: "bucket", ObjectKeyHash: hashObjectKey("enc"),
	})
	if err != nil {
		t.Fatalf("manifest get (enc): %v", err)
	}
	if man.Encryption.Mode != string(encryption.ManagedEncrypted) {
		t.Errorf("object under default Mode = %q, want managed", man.Encryption.Mode)
	}
}
