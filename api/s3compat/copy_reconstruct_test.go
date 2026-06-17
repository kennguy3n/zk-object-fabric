package s3compat

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// ecEncPlacement is an ecPlacement that also pins a gateway encryption
// mode, so a managed (or client_side) erasure-coded source can be seeded
// without a BucketConfig store.
type ecEncPlacement struct {
	backend string
	profile string
	mode    string
}

func (p ecEncPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		ErasureProfile:  p.profile,
		EncryptionMode:  p.mode,
	}, nil
}

// encPlacement pins a gateway encryption mode for single-piece and
// multipart writes (no erasure profile).
type encPlacement struct {
	backend string
	mode    string
}

func (p encPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		EncryptionMode:  p.mode,
	}, nil
}

// newReconstructCopyHandler builds a handler wired for both erasure-coded
// and multipart writes, with an advancing clock so a source and its copy
// get distinct version ids. keyring is optional (nil for plaintext /
// client_side placements, set for managed placements).
func newReconstructCopyHandler(t *testing.T, placement PlacementEngine, keyring *GatewayEncryption) (*Handler, manifest_store.ManifestStore) {
	t.Helper()
	store := memory.New()
	now := time.Unix(1700000000, 0)
	cfg := Config{
		Manifests:     store,
		Providers:     map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:     placement,
		Multipart:     multipart.NewMemoryStore(),
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Billing:       &recordingBilling{},
		Now: func() time.Time {
			cur := now
			now = now.Add(time.Second)
			return cur
		},
	}
	if keyring != nil {
		cfg.Encryption = keyring
	}
	return New(cfg), store
}

// seedMultipartSource drives a real CreateMultipartUpload → UploadPart* →
// CompleteMultipartUpload against /bucket/src so the resulting manifest is a
// genuine multi-piece multipart object (the shape CopyObject must reconstruct).
func seedMultipartSource(t *testing.T, h *Handler, parts [][]byte, createHeaders map[string]string) {
	t.Helper()
	createReq := httptest.NewRequest(http.MethodPost, "/bucket/src?uploads", nil)
	for k, v := range createHeaders {
		createReq.Header.Set(k, v)
	}
	createRec := httptest.NewRecorder()
	h.CreateMultipartUpload(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("CreateMultipartUpload = %d; body=%s", createRec.Code, createRec.Body)
	}
	var initRes initiateMultipartUploadResult
	if err := xml.Unmarshal(createRec.Body.Bytes(), &initRes); err != nil {
		t.Fatalf("decode initiate result: %v", err)
	}

	completed := make([]completeUploadEntry, 0, len(parts))
	for i, part := range parts {
		num := i + 1
		url := fmt.Sprintf("/bucket/src?uploadId=%s&partNumber=%d", initRes.UploadID, num)
		uploadReq := httptest.NewRequest(http.MethodPut, url, bytes.NewReader(part))
		uploadReq.ContentLength = int64(len(part))
		uploadRec := httptest.NewRecorder()
		h.UploadPart(uploadRec, uploadReq)
		if uploadRec.Code != http.StatusOK {
			t.Fatalf("UploadPart %d = %d; body=%s", num, uploadRec.Code, uploadRec.Body)
		}
		completed = append(completed, completeUploadEntry{
			PartNumber: num,
			ETag:       strings.Trim(uploadRec.Header().Get("ETag"), `"`),
		})
	}

	completeBody, err := xml.Marshal(completeMultipartUploadRequest{Parts: completed})
	if err != nil {
		t.Fatalf("marshal complete request: %v", err)
	}
	completeReq := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/bucket/src?uploadId=%s", initRes.UploadID), bytes.NewReader(completeBody))
	completeRec := httptest.NewRecorder()
	h.CompleteMultipartUpload(completeRec, completeReq)
	if completeRec.Code != http.StatusOK {
		t.Fatalf("CompleteMultipartUpload = %d; body=%s", completeRec.Code, completeRec.Body)
	}
}

// assertCopyRoundTrip copies /bucket/src into /bucket/dst (default COPY
// directive) and asserts the destination reads back byte-identical through
// GET. It also pins that the 501 the copy path used to return for EC /
// multipart sources is gone.
func assertCopyRoundTrip(t *testing.T, h *Handler, store manifest_store.ManifestStore, want []byte) {
	t.Helper()
	cw := copyWithHeaders(t, h, "/bucket/dst", nil)
	if cw.Code == http.StatusNotImplemented {
		t.Fatalf("copy of reconstructed source returned 501 (NotImplemented); body=%s", cw.Body)
	}
	if cw.Code != http.StatusOK {
		t.Fatalf("copy = %d, want 200; body=%s", cw.Code, cw.Body)
	}

	rec := getObjectRec(t, h, "/bucket/dst")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET dst = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if !bytes.Equal(rec.Body.Bytes(), want) {
		t.Fatalf("GET dst body mismatch: got %d bytes, want %d", rec.Body.Len(), len(want))
	}

	// The reconstructed copy must collapse the many source pieces into a
	// single destination piece (no shards, no part numbers) so subsequent
	// reads take the plain single-piece path.
	mkey := manifest_store.ManifestKey{
		TenantID:      AnonymousTenant,
		Bucket:        "bucket",
		ObjectKeyHash: hashObjectKey("dst"),
	}
	man, err := store.Get(context.Background(), mkey)
	if err != nil {
		t.Fatalf("dst manifest get: %v", err)
	}
	if len(man.Pieces) != 1 {
		t.Fatalf("dst manifest has %d pieces, want 1 (single reconstructed piece)", len(man.Pieces))
	}
	if man.Pieces[0].ShardKind != "" {
		t.Errorf("dst piece ShardKind = %q, want empty (not erasure-coded)", man.Pieces[0].ShardKind)
	}
	if man.Pieces[0].PartNumber != 0 {
		t.Errorf("dst piece PartNumber = %d, want 0 (not multipart)", man.Pieces[0].PartNumber)
	}
	if isErasureCodedManifest(man) || isMultipartManifest(man) {
		t.Errorf("dst manifest still classified as EC/multipart")
	}
}

// TestCopyObject_ReconstructsErasureCodedSource pins that CopyObject of an
// erasure-coded source — previously a hard 501 — reconstructs the object and
// re-stores it as a single-piece destination that reads back byte-identical,
// across plaintext, gateway-managed, and client_side encryption.
func TestCopyObject_ReconstructsErasureCodedSource(t *testing.T) {
	body := bytes.Repeat([]byte("ec-copy-payload!"), 4096)

	t.Run("plaintext", func(t *testing.T) {
		h, store := newReconstructCopyHandler(t,
			ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name}, nil)
		if rec := putWithHeaders(t, h, "/bucket/src", body, nil); rec.Code != http.StatusOK {
			t.Fatalf("seed EC src = %d; body=%s", rec.Code, rec.Body)
		}
		assertCopyRoundTrip(t, h, store, body)
	})

	t.Run("managed", func(t *testing.T) {
		h, store := newReconstructCopyHandler(t,
			ecEncPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name, mode: string(encryption.ManagedEncrypted)},
			newTestKeyring(t))
		if rec := putWithHeaders(t, h, "/bucket/src", body, nil); rec.Code != http.StatusOK {
			t.Fatalf("seed managed EC src = %d; body=%s", rec.Code, rec.Body)
		}
		assertCopyRoundTrip(t, h, store, body)
	})

	t.Run("client_side", func(t *testing.T) {
		h, store := newReconstructCopyHandler(t,
			ecEncPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name, mode: string(encryption.StrictZK)}, nil)
		// client_side bodies are already ciphertext to the gateway; the
		// declared algorithm header is required and the bytes are stored
		// verbatim, so the round-trip compares against the same buffer.
		hdr := map[string]string{"X-Amz-Meta-Zk-Encryption": "AES256-GCM"}
		if rec := putWithHeaders(t, h, "/bucket/src", body, hdr); rec.Code != http.StatusOK {
			t.Fatalf("seed client_side EC src = %d; body=%s", rec.Code, rec.Body)
		}
		assertCopyRoundTrip(t, h, store, body)
	})
}

// TestCopyObject_ReconstructsMultipartSource pins that CopyObject of a
// multi-part multipart source — previously a hard 501 — reconstructs the
// concatenated object and re-stores it as a single-piece destination that
// reads back byte-identical, for both plaintext and gateway-managed modes.
func TestCopyObject_ReconstructsMultipartSource(t *testing.T) {
	parts := [][]byte{
		bytes.Repeat([]byte("part-1-"), 1024),
		bytes.Repeat([]byte("part-2-payload"), 1024),
		bytes.Repeat([]byte("p3"), 777),
	}
	want := bytes.Join(parts, nil)

	t.Run("plaintext", func(t *testing.T) {
		h, store := newReconstructCopyHandler(t, fixedPlacement{backend: "test"}, nil)
		seedMultipartSource(t, h, parts, nil)
		assertCopyRoundTrip(t, h, store, want)
	})

	t.Run("managed", func(t *testing.T) {
		h, store := newReconstructCopyHandler(t,
			encPlacement{backend: "test", mode: string(encryption.ManagedEncrypted)}, newTestKeyring(t))
		seedMultipartSource(t, h, parts, nil)
		assertCopyRoundTrip(t, h, store, want)
	})
}

// TestCopyObject_ReconstructedSourcePreservesMetadata pins that the default
// COPY directive carries the source object's tags and system/user metadata
// onto the reconstructed destination, exactly like a single-piece copy.
func TestCopyObject_ReconstructedSourcePreservesMetadata(t *testing.T) {
	body := bytes.Repeat([]byte("ec-meta!"), 4096)
	h, _ := newReconstructCopyHandler(t,
		ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name}, nil)

	srcHeaders := map[string]string{
		"Content-Type":        "image/png",
		"Content-Disposition": `attachment; filename="src.png"`,
		"Cache-Control":       "max-age=600",
		"x-amz-tagging":       "team=a&env=prod",
		"x-amz-meta-owner":    "team-a",
	}
	if rec := putWithHeaders(t, h, "/bucket/src", body, srcHeaders); rec.Code != http.StatusOK {
		t.Fatalf("seed EC src = %d; body=%s", rec.Code, rec.Body)
	}

	if cw := copyWithHeaders(t, h, "/bucket/dst", nil); cw.Code != http.StatusOK {
		t.Fatalf("copy = %d, want 200; body=%s", cw.Code, cw.Body)
	}

	rec := getObjectRec(t, h, "/bucket/dst")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET dst = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	wantHeaders := map[string]string{
		"Content-Type":        "image/png",
		"Content-Disposition": `attachment; filename="src.png"`,
		"Cache-Control":       "max-age=600",
		"x-amz-meta-owner":    "team-a",
	}
	for name, val := range wantHeaders {
		if got := rec.Header().Get(name); got != val {
			t.Errorf("dst header %s = %q, want %q", name, got, val)
		}
	}

	tagRec := httptest.NewRecorder()
	h.GetObjectTagging(tagRec, httptest.NewRequest(http.MethodGet, "/bucket/dst?tagging", nil))
	if tagRec.Code != http.StatusOK {
		t.Fatalf("GET dst ?tagging = %d, want 200; body=%s", tagRec.Code, tagRec.Body)
	}
	for _, kv := range []string{"<Key>team</Key>", "<Value>a</Value>", "<Key>env</Key>", "<Value>prod</Value>"} {
		if !strings.Contains(tagRec.Body.String(), kv) {
			t.Errorf("dst tagging missing %q; body=%s", kv, tagRec.Body)
		}
	}
}
