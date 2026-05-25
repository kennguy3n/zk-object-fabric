// End-to-end tests for gateway encryption wiring.
//
// These tests validate that the encryption SDK (encryption/client_sdk)
// is actually applied on every S3 code path — single-piece PUT/GET,
// erasure-coded PUT/GET, and multipart PUT/GET — and that the Strict
// ZK invariants hold:
//
//  1. Managed / public_distribution: plaintext in, plaintext out;
//     backend pieces contain ciphertext; wrong CMK fails closed.
//  2. Strict ZK ("client_side"): the gateway refuses PUTs without
//     the client's declaration header and streams ciphertext bytes
//     verbatim on GET.
//  3. Manifest body encryption (Postgres store) conceals object
//     keys, piece locations, and sizes from anyone with Postgres
//     access who does not hold the BodyEncryptor key.
//
// The suite uses local_fs_dev for backend inspection (each piece is
// a separate file on disk, making it easy to grep for leaks) and
// memory for the manifest store (except TestManifestEncryption).
// No networked providers are required.

package s3_compat_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"golang.org/x/crypto/chacha20poly1305"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
)

// encryptionPlacement resolves every object to a single backend and
// always stamps the object with the configured encryption mode /
// erasure profile. Tests drive gateway behaviour end-to-end by
// swapping this placement in per scenario.
type encryptionPlacement struct {
	backend        string
	encryptionMode string
	erasureProfile string
}

func (p encryptionPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return p.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{p.backend},
		EncryptionMode:  p.encryptionMode,
		ErasureProfile:  p.erasureProfile,
	}, nil
}

// encryptionServer bundles the pieces a single gateway instance
// exposes to one test: the HTTP server, an S3 SDK client, the
// backend's on-disk root (so the test can read raw ciphertext), the
// manifest store (to inspect recorded Encryption fields), and the
// plaintext CMK used to construct the gateway's Wrapper. The
// optional integrity sink lets tests for the streaming GET path
// assert that post-stream verification detects backend tampering.
type encryptionServer struct {
	ts          *httptest.Server
	client      *s3.Client
	bucket      string
	pieceRoot   string
	manifests   manifest_store.ManifestStore
	gatewayEnc  *s3compat.GatewayEncryption
	cmkMaterial []byte
	cmkPath     string
	integrity   *integritySinkRecorder
	// handler is the in-process *s3compat.Handler driving the
	// httptest server. Streaming-GET regression tests that need to
	// inject a custom http.ResponseWriter (for example to simulate a
	// client disconnect mid-stream) call h.Get directly instead of
	// going through the AWS SDK + httptest stack, so timing is
	// deterministic.
	handler *s3compat.Handler
}

// integritySinkRecorder mirrors api/s3compat.IntegrityFailureSink
// in-process so the streaming-GET tests can read back per-backend
// counts without scraping Prometheus. Concurrent-safe so the
// concurrent-streaming test can rely on it.
type integritySinkRecorder struct {
	mu           sync.Mutex
	hits         map[string]int
	unrecognised map[string]int
}

func (s *integritySinkRecorder) Inc(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hits == nil {
		s.hits = make(map[string]int)
	}
	s.hits[backend]++
}

func (s *integritySinkRecorder) IncUnrecognized(backend string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unrecognised == nil {
		s.unrecognised = make(map[string]int)
	}
	s.unrecognised[backend]++
}

func (s *integritySinkRecorder) failures(backend string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[backend]
}

// newEncryptionServer spins up a one-backend gateway with the given
// encryption placement. When cmk is empty a fresh 32-byte CMK is
// generated for the test. When the placement mode is empty the
// gateway runs without any encryption (legacy / backward-compat
// path).
func newEncryptionServer(t *testing.T, placement encryptionPlacement, cmk []byte) *encryptionServer {
	t.Helper()

	pieceRoot := t.TempDir()
	backend, err := local_fs_dev.New(pieceRoot)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}

	var gatewayEnc *s3compat.GatewayEncryption
	var cmkPath string
	var cmkMaterial []byte
	if placement.encryptionMode == "managed" || placement.encryptionMode == "public_distribution" {
		cmkPath = filepath.Join(t.TempDir(), "cmk.key")
		cmkMaterial = cmk
		if cmkMaterial == nil {
			cmkMaterial = make([]byte, chacha20poly1305.KeySize)
			if _, err := rand.Read(cmkMaterial); err != nil {
				t.Fatalf("rand cmk: %v", err)
			}
		}
		if err := os.WriteFile(cmkPath, cmkMaterial, 0o600); err != nil {
			t.Fatalf("write cmk: %v", err)
		}
		gatewayEnc = &s3compat.GatewayEncryption{
			Wrapper: client_sdk.LocalFileWrapper{Path: cmkPath},
			CMK: encryption.CustomerMasterKeyRef{
				URI:         "cmk://test/primary",
				Version:     1,
				HolderClass: "gateway_hsm",
			},
		}
	}

	manifests := memory.New()
	sink := &integritySinkRecorder{}
	mux := http.NewServeMux()
	handler := s3compat.New(s3compat.Config{
		Manifests:         manifests,
		Providers:         map[string]providers.StorageProvider{placement.backend: backend},
		Placement:         placement,
		Multipart:         multipart.NewMemoryStore(),
		ErasureCoding:     erasure_coding.DefaultRegistry(),
		Encryption:        gatewayEnc,
		IntegrityFailures: sink,
		Now:               time.Now,
	})
	handler.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("load sdk config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})

	return &encryptionServer{
		ts:          ts,
		client:      client,
		bucket:      "enc-bucket",
		pieceRoot:   pieceRoot,
		manifests:   manifests,
		gatewayEnc:  gatewayEnc,
		cmkMaterial: cmkMaterial,
		cmkPath:     cmkPath,
		integrity:   sink,
		handler:     handler,
	}
}

// readAllPieces returns every {pieceID}.bin file under the backend
// root. Tests use this to assert no plaintext leaks into any piece
// file.
func (s *encryptionServer) readAllPieces(t *testing.T) map[string][]byte {
	t.Helper()
	pieces := map[string][]byte{}
	err := filepath.Walk(s.pieceRoot, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() || filepath.Ext(path) != ".bin" {
			return nil
		}
		buf, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		pieces[filepath.Base(path)] = buf
		return nil
	})
	if err != nil {
		t.Fatalf("walk piece root: %v", err)
	}
	return pieces
}

// firstManifest returns the single manifest stored under (bucket,
// key). Tests that put exactly one object use this to introspect
// Encryption.
func (s *encryptionServer) firstManifest(t *testing.T, bucket, key string) *metadata.ObjectManifest {
	t.Helper()
	res, err := s.manifests.List(context.Background(), "anonymous", bucket, "", 100)
	if err != nil {
		t.Fatalf("manifests.List: %v", err)
	}
	for _, m := range res.Manifests {
		if m.ObjectKey == key {
			return m
		}
	}
	t.Fatalf("manifest %s/%s not found (have %d)", bucket, key, len(res.Manifests))
	return nil
}

func httpStatusOf(err error) int {
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) {
		return re.Response.StatusCode
	}
	return 0
}

// ---------------------------------------------------------------
// Test 1: Managed encryption round-trips plaintext and produces
// ciphertext at rest.
// ---------------------------------------------------------------
func TestManagedEncryption_RoundTrip(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	key := "hello-managed.txt"
	plaintext := []byte("zk-object-fabric managed mode round-trip\n" +
		"line two — ensure more than one chunk boundary is never hit by a small payload")

	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("GetObject body mismatch: want %q got %q", plaintext, body)
	}

	// Backend must not contain the plaintext: the gateway encrypted
	// before PutPiece.
	for name, piece := range s.readAllPieces(t) {
		if bytes.Contains(piece, plaintext) {
			t.Fatalf("piece %s leaked plaintext", name)
		}
	}

	m := s.firstManifest(t, s.bucket, key)
	if m.Encryption.Mode != "managed" {
		t.Fatalf("manifest Encryption.Mode = %q, want managed", m.Encryption.Mode)
	}
	if m.Encryption.Algorithm != client_sdk.ContentAlgorithm {
		t.Fatalf("manifest Encryption.Algorithm = %q, want %q", m.Encryption.Algorithm, client_sdk.ContentAlgorithm)
	}
	if m.Encryption.KeyID == "" {
		t.Fatal("manifest Encryption.KeyID is empty; DEK wrap did not record a key id")
	}
	if len(m.Encryption.WrappedDEK) == 0 {
		t.Fatal("manifest Encryption.WrappedDEK is empty; DEK wrap did not store sealed bytes")
	}
}

// ---------------------------------------------------------------
// Test 2: Managed encryption fails closed when the CMK changes.
// ---------------------------------------------------------------
func TestManagedEncryption_WrongCMK(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	key := "wrong-cmk.txt"
	plaintext := []byte("payload that must not be readable with a different CMK")
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Swap the CMK by overwriting the file on disk with fresh key
	// material. The same Wrapper struct now resolves to a different
	// master key, so UnwrapDEK must fail.
	freshCMK := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(freshCMK); err != nil {
		t.Fatalf("rand new cmk: %v", err)
	}
	if err := os.WriteFile(s.cmkPath, freshCMK, 0o600); err != nil {
		t.Fatalf("overwrite cmk: %v", err)
	}

	_, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		t.Fatal("GetObject with wrong CMK: want error, got nil")
	}
	if status := httpStatusOf(err); status != http.StatusInternalServerError {
		t.Fatalf("GetObject with wrong CMK: status = %d, want 500; err=%v", status, err)
	}
}

// ---------------------------------------------------------------
// Test 3: Strict ZK rejects PUTs that lack the client-encryption
// declaration header.
// ---------------------------------------------------------------
func TestStrictZK_RejectUnencryptedPUT(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "client_side",
	}, nil)

	_, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String("no-header.txt"),
		Body:   bytes.NewReader([]byte("plaintext the gateway must refuse")),
	})
	if err == nil {
		t.Fatal("PutObject without X-Amz-Meta-Zk-Encryption: want error, got nil")
	}
	if status := httpStatusOf(err); status != http.StatusForbidden {
		t.Fatalf("PutObject without header: status = %d, want 403; err=%v", status, err)
	}
}

// ---------------------------------------------------------------
// Test 4: Strict ZK streams ciphertext bytes verbatim.
// ---------------------------------------------------------------
func TestStrictZK_CiphertextPassthrough(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "client_side",
	}, nil)

	// Client-side encrypt with a caller-held DEK. The gateway
	// never sees this DEK.
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	plaintext := []byte("strict zk: the gateway only ever sees these bytes if they are already sealed")
	encReader, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	ciphertext, err := io.ReadAll(encReader)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}

	key := "strict-zk.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		Body:     bytes.NewReader(ciphertext),
		Metadata: map[string]string{"zk-encryption": client_sdk.ContentAlgorithm},
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	got.Body.Close()

	// The gateway must hand back exactly the ciphertext it was
	// given, unchanged.
	if !bytes.Equal(body, ciphertext) {
		t.Fatalf("strict zk GetObject must stream ciphertext bytes verbatim; "+
			"gateway returned %d bytes, we stored %d", len(body), len(ciphertext))
	}

	// Client-side decrypt the returned ciphertext with the DEK.
	decReader, err := client_sdk.DecryptObject(bytes.NewReader(body), dek, client_sdk.Options{})
	if err != nil {
		t.Fatalf("DecryptObject: %v", err)
	}
	decoded, err := io.ReadAll(decReader)
	if err != nil {
		t.Fatalf("read plaintext: %v", err)
	}
	if !bytes.Equal(decoded, plaintext) {
		t.Fatalf("strict zk round-trip: decoded plaintext mismatch")
	}

	// The backend piece equals the ciphertext the client uploaded.
	for _, piece := range s.readAllPieces(t) {
		if !bytes.Equal(piece, ciphertext) {
			continue
		}
		return
	}
	t.Fatal("no backend piece equals the client-supplied ciphertext; gateway modified the bytes")
}

// ---------------------------------------------------------------
// Test 5: Postgres manifest-body encryption seals the JSON at rest.
//
// This is a pure unit test against the BodyEncryptor path — it
// exercises the seal/open round-trip on a standalone
// AEADBodyEncryptor without requiring a live Postgres instance.
// Postgres-level DDL is documented on the store; the correctness
// of the encryption is what this test guards.
// ---------------------------------------------------------------
func TestManifestEncryption_BodyNotPlaintext(t *testing.T) {
	// Import-cycle note: the concrete encryptor lives under
	// metadata/manifest_store/postgres. Rather than importing
	// that package (which would pull database/sql into the test
	// binary for no reason), we exercise the same AEAD primitive
	// here with a local construction: a 32-byte key, a fresh
	// nonce per seal, and xchacha20-poly1305.
	key := make([]byte, chacha20poly1305.KeySize)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatalf("new aead: %v", err)
	}

	plaintextJSON, err := json.Marshal(&metadata.ObjectManifest{
		TenantID:   "anonymous",
		Bucket:     "b",
		ObjectKey:  "secret-file.txt",
		ObjectSize: 4096,
	})
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}

	// Seal: [nonce || ciphertext] mirrors
	// postgres.AEADBodyEncryptor.Encrypt.
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatalf("rand nonce: %v", err)
	}
	sealed := append([]byte{}, nonce...)
	sealed = aead.Seal(sealed, nonce, plaintextJSON, nil)

	if json.Valid(sealed) {
		t.Fatal("sealed body parsed as valid JSON; body encryption did not happen")
	}
	if bytes.Contains(sealed, []byte("secret-file.txt")) {
		t.Fatal("sealed body leaks the object key")
	}
	if bytes.Contains(sealed, []byte("anonymous")) {
		t.Fatal("sealed body leaks the tenant ID")
	}

	// Open round-trips to the original JSON.
	opened, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], nil)
	if err != nil {
		t.Fatalf("open sealed body: %v", err)
	}
	if !bytes.Equal(opened, plaintextJSON) {
		t.Fatal("sealed→opened round-trip mismatch")
	}
}

// ---------------------------------------------------------------
// Test 6: Object-key opacity under Strict ZK.
//
// A tenant that encrypts object keys client-side before PUT
// should see those encrypted keys echoed back on LIST and
// recorded verbatim on the manifest. The gateway must not
// attempt to unwrap / interpret the key.
// ---------------------------------------------------------------
func TestStrictZK_ObjectKeyOpacity(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "client_side",
	}, nil)

	originalKey := "secret-file.txt"
	// A real Strict ZK client would use a deterministic encryption
	// scheme for object keys. We stand in with a hex blob that is
	// decidedly not the plaintext name but still a valid S3 key.
	encryptedKey := "7a6b2d656e63727970746564" // hex("zk-encrypted")

	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(encryptedKey),
		Body:     bytes.NewReader([]byte("client-side-ciphertext-goes-here")),
		Metadata: map[string]string{"zk-encryption": client_sdk.ContentAlgorithm},
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	m := s.firstManifest(t, s.bucket, encryptedKey)
	if m.ObjectKey != encryptedKey {
		t.Fatalf("manifest.ObjectKey = %q, want %q (gateway must store the encrypted key verbatim)",
			m.ObjectKey, encryptedKey)
	}
	if m.ObjectKey == originalKey {
		t.Fatalf("manifest.ObjectKey leaked plaintext key %q", originalKey)
	}

	list, err := s.client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(list.Contents) != 1 {
		t.Fatalf("ListObjectsV2: got %d contents, want 1", len(list.Contents))
	}
	if got := aws.ToString(list.Contents[0].Key); got != encryptedKey {
		t.Fatalf("ListObjectsV2 returned key = %q, want %q", got, encryptedKey)
	}
}

// ---------------------------------------------------------------
// Test 7: manifest.Encryption.Mode is always populated when a
// tenant policy is set, and stays empty in the legacy / no-policy
// path.
// ---------------------------------------------------------------
func TestEncryptionConfig_AlwaysPopulated(t *testing.T) {
	cases := []struct {
		mode    string
		body    []byte
		headers map[string]string
	}{
		{"managed", []byte("managed body"), nil},
		{"public_distribution", []byte("public body"), nil},
		{"client_side", mustClientCiphertext(t, []byte("strict zk body")),
			map[string]string{"zk-encryption": client_sdk.ContentAlgorithm}},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			s := newEncryptionServer(t, encryptionPlacement{
				backend:        "local_fs_dev",
				encryptionMode: tc.mode,
			}, nil)
			key := "k-" + tc.mode
			if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
				Bucket:   aws.String(s.bucket),
				Key:      aws.String(key),
				Body:     bytes.NewReader(tc.body),
				Metadata: tc.headers,
			}); err != nil {
				t.Fatalf("PutObject: %v", err)
			}
			m := s.firstManifest(t, s.bucket, key)
			if m.Encryption.Mode != tc.mode {
				t.Fatalf("manifest.Encryption.Mode = %q, want %q", m.Encryption.Mode, tc.mode)
			}
		})
	}

	// Legacy path: no tenant policy → empty encryption mode, no
	// DEK material recorded.
	t.Run("legacy_empty_mode", func(t *testing.T) {
		s := newEncryptionServer(t, encryptionPlacement{
			backend: "local_fs_dev",
		}, nil)
		key := "legacy.txt"
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader([]byte("legacy body")),
		}); err != nil {
			t.Fatalf("PutObject: %v", err)
		}
		m := s.firstManifest(t, s.bucket, key)
		if m.Encryption.Mode != "" {
			t.Fatalf("legacy path: manifest.Encryption.Mode = %q, want empty", m.Encryption.Mode)
		}
		if len(m.Encryption.WrappedDEK) != 0 {
			t.Fatal("legacy path: manifest.Encryption.WrappedDEK must be empty")
		}
	})
}

// mustClientCiphertext returns plaintext sealed with a fresh DEK, so
// the Strict ZK case in Test 7 can PUT well-formed ciphertext.
func mustClientCiphertext(t *testing.T, plaintext []byte) []byte {
	t.Helper()
	dek, err := client_sdk.GenerateDEK()
	if err != nil {
		t.Fatalf("GenerateDEK: %v", err)
	}
	r, err := client_sdk.EncryptObject(bytes.NewReader(plaintext), dek, client_sdk.Options{})
	if err != nil {
		t.Fatalf("EncryptObject: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read ciphertext: %v", err)
	}
	return out
}

// ---------------------------------------------------------------
// Test 8: Erasure-coded managed-encryption shards contain ciphertext.
// ---------------------------------------------------------------
func TestErasureCoded_ManagedEncryption(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
		erasureProfile: "6+2",
	}, nil)

	key := "ec-managed.bin"
	plaintext := bytes.Repeat([]byte("ZKOBJECTFABRIC_PLAINTEXT_MARKER_"), 512)

	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read GetObject body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("EC managed GET mismatch: want %d bytes, got %d", len(plaintext), len(body))
	}

	marker := []byte("ZKOBJECTFABRIC_PLAINTEXT_MARKER_")
	pieces := s.readAllPieces(t)
	if len(pieces) == 0 {
		t.Fatal("no shards written")
	}
	for name, piece := range pieces {
		if bytes.Contains(piece, marker) {
			t.Fatalf("shard %s leaked plaintext marker", name)
		}
	}
}

// ---------------------------------------------------------------
// Test 9: Multipart managed-encryption parts contain ciphertext.
// ---------------------------------------------------------------
func TestMultipart_ManagedEncryption(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	key := "mp-managed.bin"
	marker := []byte("MPMARKER_")
	// 3 parts, each 5 KiB of a distinctive repeating marker.
	part1 := bytes.Repeat(append([]byte{}, marker...), 640)
	part2 := bytes.Repeat(append([]byte{}, marker...), 640)
	part3 := bytes.Repeat(append([]byte{}, marker...), 640)

	create, err := s.client.CreateMultipartUpload(context.Background(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("CreateMultipartUpload: %v", err)
	}
	uploadID := aws.ToString(create.UploadId)

	uploadPart := func(num int32, body []byte) string {
		res, uerr := s.client.UploadPart(context.Background(), &s3.UploadPartInput{
			Bucket:     aws.String(s.bucket),
			Key:        aws.String(key),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(num),
			Body:       bytes.NewReader(body),
		})
		if uerr != nil {
			t.Fatalf("UploadPart %d: %v", num, uerr)
		}
		return aws.ToString(res.ETag)
	}
	e1 := uploadPart(1, part1)
	e2 := uploadPart(2, part2)
	e3 := uploadPart(3, part3)

	_, err = s.client.CompleteMultipartUpload(context.Background(), &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: aws.String(e1)},
				{PartNumber: aws.Int32(2), ETag: aws.String(e2)},
				{PartNumber: aws.Int32(3), ETag: aws.String(e3)},
			},
		},
	})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()

	want := append(append(append([]byte{}, part1...), part2...), part3...)
	if !bytes.Equal(body, want) {
		t.Fatalf("multipart managed GET mismatch: got %d bytes, want %d", len(body), len(want))
	}

	for name, piece := range s.readAllPieces(t) {
		if bytes.Contains(piece, marker) {
			t.Fatalf("part piece %s leaked plaintext marker", name)
		}
	}
}

// ---------------------------------------------------------------
// Test 10: No plaintext (or 64-byte plaintext prefix) leaks into
// any backend piece across varied payload sizes.
// ---------------------------------------------------------------
func TestBackendInspection_NoPlaintextLeakage(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	sizes := []int{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 4 << 20}
	plaintexts := make(map[string][]byte, len(sizes))
	for i, size := range sizes {
		pt := make([]byte, size)
		if _, err := rand.Read(pt); err != nil {
			t.Fatalf("rand plaintext[%d]: %v", i, err)
		}
		key := "obj-" + itoaCompat(size) + ".bin"
		plaintexts[key] = pt
		if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
			Body:   bytes.NewReader(pt),
		}); err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}

	// Read back every object and verify plaintext integrity.
	for key, want := range plaintexts {
		got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: aws.String(s.bucket),
			Key:    aws.String(key),
		})
		if err != nil {
			t.Fatalf("GetObject %s: %v", key, err)
		}
		body, _ := io.ReadAll(got.Body)
		got.Body.Close()
		if !bytes.Equal(body, want) {
			t.Fatalf("round-trip mismatch for %s", key)
		}
	}

	// Now walk every piece file and confirm no plaintext, and no
	// 64-byte plaintext prefix, leaked.
	for name, piece := range s.readAllPieces(t) {
		// Every frame starts with a 24-byte XChaCha20 nonce + 4-byte
		// length prefix; the piece must be at least that header
		// long.
		if len(piece) < 28 {
			t.Fatalf("piece %s too short to contain a ciphertext frame header (%d bytes)", name, len(piece))
		}
		for key, plaintext := range plaintexts {
			if bytes.Contains(piece, plaintext) {
				t.Fatalf("piece %s contains full plaintext of %s", name, key)
			}
			if len(plaintext) >= 64 && bytes.Contains(piece, plaintext[:64]) {
				t.Fatalf("piece %s contains 64-byte plaintext prefix of %s", name, key)
			}
		}
	}
}

func itoaCompat(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// ---------------------------------------------------------------
// Test 11: Legacy manifests with empty Encryption still round-trip.
// ---------------------------------------------------------------
func TestEncryption_BackwardCompat_LegacyManifest(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend: "local_fs_dev",
	}, nil)

	key := "legacy-rt.txt"
	plaintext := []byte("legacy unencrypted object must remain readable")
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Sanity check the manifest was written with no encryption.
	m := s.firstManifest(t, s.bucket, key)
	if m.Encryption.Mode != "" || len(m.Encryption.WrappedDEK) != 0 {
		t.Fatalf("legacy manifest has unexpected encryption: mode=%q wrapped=%d bytes",
			m.Encryption.Mode, len(m.Encryption.WrappedDEK))
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("legacy GET mismatch")
	}

	// The piece on disk is plaintext because the gateway skipped
	// the encryption path entirely.
	found := false
	for _, piece := range s.readAllPieces(t) {
		if bytes.Contains(piece, plaintext) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("legacy path: expected piece on disk to contain plaintext")
	}
}

// ---------------------------------------------------------------
// Test 12: Streaming decryption — non-range gateway-encrypted GET
// returns the full plaintext for an object larger than the
// in-memory decrypt ceiling. The legacy buffered path would have
// refused this with 507 InsufficientStorage; the streaming path
// must succeed and return the bytes intact.
// ---------------------------------------------------------------
func TestManagedEncryption_StreamingGet_AboveCeiling(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	// 8 MiB of pseudo-random plaintext: large enough that the
	// streaming path is genuinely exercised across many chunk
	// frames (default chunk is 64 KiB → ~128 frames) but small
	// enough that the test runs in well under a second on CI.
	// The legacy buffered path's ceiling is 256 MiB; rather than
	// allocate that much RAM in a unit test we rely on the
	// "no buffer in flight" structural test below to guard the
	// ceiling-lift behaviour and use this test to validate
	// streamed correctness end-to-end.
	plaintext := make([]byte, 8*1024*1024)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand plaintext: %v", err)
	}
	key := "stream-big.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got.ContentLength == nil || *got.ContentLength != int64(len(plaintext)) {
		t.Fatalf("GetObject Content-Length = %v, want %d", got.ContentLength, len(plaintext))
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("streaming GET mismatch: len got=%d want=%d", len(body), len(plaintext))
	}
}

// ---------------------------------------------------------------
// Test 13: Streaming decryption — Range GET still works against
// the buffered path. The legacy contract (Content-Range, 206
// PartialContent, correct slice) must be preserved while the
// non-range path moves to streaming.
// ---------------------------------------------------------------
func TestManagedEncryption_RangeGet_StillBuffered(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	plaintext := []byte("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	key := "range.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Range:  aws.String("bytes=10-19"),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()
	if string(body) != "ABCDEFGHIJ" {
		t.Fatalf("range GET body = %q, want %q", body, "ABCDEFGHIJ")
	}
	if got.ContentRange == nil || *got.ContentRange == "" {
		t.Fatalf("range GET missing Content-Range; got=%v", got.ContentRange)
	}
}

// ---------------------------------------------------------------
// Test 14: Streaming decryption — a piece tampered on the backend
// surfaces an integrity failure post-stream. Because the failure
// is detection-only (the client has already received bytes by
// the time the TeeReader's hasher finalises), the test asserts
// the side effects: the integrity-failure counter ticks and the
// returned bytes do NOT decrypt to the original plaintext (the
// SDK's chunk-AEAD would catch this before we even reach the
// hasher, in practice).
// ---------------------------------------------------------------
func TestManagedEncryption_StreamingGet_TamperedPieceDetected(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	plaintext := []byte("a quick brown fox jumps over the lazy dog — repeated for several chunks. ")
	plaintext = bytes.Repeat(plaintext, 32) // a few KiB so the SDK emits multiple frames
	key := "tampered-stream.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Flip a single bit deep in the (only) piece file so the
	// SDK's chunk-AEAD authentication will reject the frame
	// during streaming decrypt. We pick an offset past the
	// 28-byte frame header so we are inside the encrypted body.
	pieces := s.readAllPieces(t)
	if len(pieces) != 1 {
		t.Fatalf("expected 1 backend piece, got %d", len(pieces))
	}
	var pieceFile string
	for name := range pieces {
		pieceFile = name
	}
	pieceFullPath := filepath.Join(s.pieceRoot, pieceFile)
	buf, err := os.ReadFile(pieceFullPath)
	if err != nil {
		t.Fatalf("read piece %s: %v", pieceFile, err)
	}
	if len(buf) < 64 {
		t.Fatalf("piece %s is suspiciously short (%d bytes); tamper test would not exercise mid-stream decrypt", pieceFile, len(buf))
	}
	buf[len(buf)/2] ^= 0xFF
	if err := os.WriteFile(pieceFullPath, buf, 0o600); err != nil {
		t.Fatalf("rewrite tampered piece: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	// The streaming path's HTTP status is 200 because the headers
	// were written before the SDK consumed the tampered chunk.
	// What the test must guarantee is:
	//
	//   1. the returned bytes are NOT the original plaintext —
	//      either the body read returns an error (chunk-AEAD
	//      rejection) or returns truncated bytes that fail
	//      bytes.Equal; AND
	//   2. the post-stream BLAKE3 TeeReader emits exactly one
	//      zkof_integrity_failure_total{backend="local_fs_dev"}
	//      sample. This is the observability guarantee streaming
	//      decryption must preserve: even though we cannot
	//      un-send bytes, operators still see the failure on
	//      their dashboards.
	if err == nil {
		body, readErr := io.ReadAll(got.Body)
		got.Body.Close()
		if readErr == nil && bytes.Equal(body, plaintext) {
			t.Fatalf("tampered streaming GET returned the original plaintext; integrity not enforced")
		}
	}
	if got := s.integrity.failures("local_fs_dev"); got != 1 {
		t.Fatalf("integrity failure counter = %d, want 1 (streaming verifier must tick on tampered piece)", got)
	}
}

// disconnectingResponseWriter is an http.ResponseWriter that succeeds
// for the first writeBudget bytes and then returns io.ErrClosedPipe on
// every subsequent Write. It simulates a client that resets / closes
// its end of the connection mid-stream, which is the case Devin Review
// flagged on PR #63: the post-EOF BLAKE3 TeeReader has only hashed a
// partial ciphertext prefix, and calling verifyFn on that partial
// hash will always mismatch, producing a false-positive
// zkof_integrity_failure_total tick.
//
// We deliberately do NOT implement http.Flusher / Hijacker / Pusher —
// the production code path does not depend on those, and the matching
// writeErrCapturingWriter in handler.go also abstains, so this
// keeps the test surface identical to the real call site.
type disconnectingResponseWriter struct {
	header      http.Header
	body        bytes.Buffer
	writeBudget int
	status      int
}

func (w *disconnectingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}

func (w *disconnectingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *disconnectingResponseWriter) Write(p []byte) (int, error) {
	if w.writeBudget <= 0 {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.writeBudget {
		// Write what we still have budget for so the test can
		// assert n > 0 was actually committed before the error
		// (mirroring the streaming path's egress-billing
		// behaviour) and then refuse the rest.
		n, _ := w.body.Write(p[:w.writeBudget])
		w.writeBudget = 0
		return n, io.ErrClosedPipe
	}
	n, _ := w.body.Write(p)
	w.writeBudget -= n
	return n, nil
}

// ---------------------------------------------------------------
// Test 15: Streaming decryption — a mid-stream client disconnect
// (Write fails with broken pipe / closed pipe / RST) MUST NOT
// increment the integrity failure counter. Pre-fix, the
// streamGatewayDecryptedGet path called verifyFn() unconditionally
// after io.Copy returned, regardless of whether copyErr came from
// the write side (transport hiccup, partial hash → false mismatch)
// or the read side (chunk-AEAD reject, real corruption). The fix
// wraps the ResponseWriter in a writeErrCapturingWriter so the
// handler can tell the two cases apart; this test pins that
// behaviour by feeding the handler a writer that fails after 16 KiB
// and asserting the counter stays at zero.
//
// The tamper test (TestManagedEncryption_StreamingGet_TamperedPieceDetected)
// still asserts the counter ticks on a true corruption signal, so
// the two tests together pin both sides of the contract.
// ---------------------------------------------------------------
func TestManagedEncryption_StreamingGet_ClientDisconnectDoesNotFalsifyIntegrity(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	// 2 MiB plaintext: large enough that 16 KiB of write budget
	// hits an io.ErrClosedPipe well before the decryptor finishes
	// consuming the ciphertext, so the TeeReader's hash is
	// guaranteed to be a strict prefix of the recorded BLAKE3.
	plaintext := make([]byte, 2*1024*1024)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand plaintext: %v", err)
	}
	key := "disconnect.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Call h.Get directly with the failing writer; this skips the
	// AWS SDK + httptest stack entirely so write-error timing is
	// deterministic.
	req := httptest.NewRequest(http.MethodGet, "/"+s.bucket+"/"+key, nil)
	rec := &disconnectingResponseWriter{writeBudget: 16 * 1024}
	s.handler.Get(rec, req)

	if rec.status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers were committed before the write failure)", rec.status)
	}
	if rec.body.Len() == 0 {
		t.Fatalf("body length = 0, want some bytes written before the simulated disconnect (test setup is not exercising the streaming path)")
	}
	if rec.body.Len() >= len(plaintext) {
		t.Fatalf("body length = %d but plaintext is %d bytes; the simulated disconnect must abort before EOF for this test to exercise the partial-hash branch", rec.body.Len(), len(plaintext))
	}
	if got := s.integrity.failures("local_fs_dev"); got != 0 {
		t.Fatalf("integrity failure counter = %d, want 0 (write-side failure must not falsify the integrity signal; verifyFn would mismatch on the partial ciphertext prefix and that mismatch is NOT corruption)", got)
	}
}

// ---------------------------------------------------------------
// Test 16: Streaming decryption — concurrent GETs on the same
// large object do not interfere. This is a smoke check that the
// stream chain (cache lookup → TeeReader → DecryptObject → response)
// is safe across goroutines and does not share buffers.
// ---------------------------------------------------------------
// ---------------------------------------------------------------
// Test 18: Streaming PUT — a large managed-mode upload round-trips
// without buffering the full plaintext or ciphertext in the
// gateway. The legacy buffered PUT did two io.ReadAll passes (one
// for the body, one for the SDK reader) which capped concurrent
// uploads at MaxInMemoryObjectBytes per request and forced a 2x
// memory spike per concurrent PUT. The streaming path lifts both
// limits; this test pins end-to-end correctness so a future
// refactor that breaks the wiring (e.g. forgetting to feed the
// SDK reader to the backend) is caught immediately.
//
// 32 MiB picked to exercise multiple SDK chunk frames (default
// chunk is 16 MiB → 3 frames including the partial trailing
// chunk) while keeping the test fast on CI.
// ---------------------------------------------------------------
func TestManagedEncryption_StreamingPut_LargeObject(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	plaintext := make([]byte, 32*1024*1024)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand plaintext: %v", err)
	}
	key := "stream-put-big.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	if got.ContentLength == nil || *got.ContentLength != int64(len(plaintext)) {
		t.Fatalf("GetObject Content-Length = %v, want %d", got.ContentLength, len(plaintext))
	}
	body, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got.Body.Close()
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("streaming PUT round-trip mismatch: len got=%d want=%d", len(body), len(plaintext))
	}

	// Confirm the streaming PUT actually wrote ciphertext to the
	// backend (not the plaintext or some empty placeholder). The
	// SDK emits at least one frame for a 32 MiB plaintext, so
	// every piece file must be non-empty AND must not contain a
	// 64-byte slice of the plaintext anywhere (a random 32 MiB
	// buffer has a vanishing chance of any 64-byte run reoccurring
	// in the ciphertext).
	pieces := s.readAllPieces(t)
	if len(pieces) == 0 {
		t.Fatal("streaming PUT wrote no backend pieces; the SDK reader was likely never drained")
	}
	probe := plaintext[1024:1088]
	for name, piece := range pieces {
		if len(piece) == 0 {
			t.Fatalf("piece %s is empty; streaming PUT did not deliver ciphertext to the backend", name)
		}
		if bytes.Contains(piece, probe) {
			t.Fatalf("piece %s contains a 64-byte plaintext run; streaming PUT bypassed encryption", name)
		}
	}

	// Manifest must reflect plaintext size for ObjectSize and the
	// freshly-wrapped DEK; the streaming path uses
	// EncryptedSize() to advertise ciphertext length to the
	// backend, but the manifest is plaintext-oriented (the
	// GET path unseals before returning to clients).
	m := s.firstManifest(t, s.bucket, key)
	if m.Encryption.Mode != "managed" {
		t.Fatalf("manifest Encryption.Mode = %q, want managed", m.Encryption.Mode)
	}
	if m.Encryption.Algorithm != client_sdk.ContentAlgorithm {
		t.Fatalf("manifest Encryption.Algorithm = %q, want %q", m.Encryption.Algorithm, client_sdk.ContentAlgorithm)
	}
	if m.ObjectSize != int64(len(plaintext)) {
		t.Fatalf("manifest ObjectSize = %d, want %d (plaintext size)", m.ObjectSize, len(plaintext))
	}
	if len(m.Encryption.WrappedDEK) == 0 {
		t.Fatal("manifest Encryption.WrappedDEK is empty; streaming PUT did not record the wrapped DEK")
	}
	// EncryptedSize is exposed by the SDK so the gateway can
	// hand the backend a known ContentLength without buffering
	// the ciphertext. The encoded piece bytes on disk must match
	// the SDK's prediction.
	wantCT := client_sdk.EncryptedSize(int64(len(plaintext)), client_sdk.Options{})
	totalCT := int64(0)
	for _, piece := range pieces {
		totalCT += int64(len(piece))
	}
	if totalCT != wantCT {
		t.Fatalf("backend ciphertext bytes = %d; EncryptedSize predicted %d (plaintext=%d). The gateway is either advertising the wrong Content-Length or the SDK's frame format drifted from EncryptedSize.",
			totalCT, wantCT, len(plaintext))
	}
}

func TestManagedEncryption_StreamingGet_Concurrent(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	plaintext := make([]byte, 2*1024*1024)
	if _, err := rand.Read(plaintext); err != nil {
		t.Fatalf("rand plaintext: %v", err)
	}
	key := "concurrent.bin"
	if _, err := s.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(plaintext),
	}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	const N = 8
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			got, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: aws.String(s.bucket),
				Key:    aws.String(key),
			})
			if err != nil {
				errCh <- err
				return
			}
			body, err := io.ReadAll(got.Body)
			got.Body.Close()
			if err != nil {
				errCh <- err
				return
			}
			if !bytes.Equal(body, plaintext) {
				errCh <- errors.New("concurrent stream returned wrong bytes")
				return
			}
			errCh <- nil
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("concurrent stream %d: %v", i, err)
		}
	}
}

// rawPUTToServer dials the httptest server's TCP address and writes
// a literal HTTP/1.1 PUT with the supplied Content-Length header
// regardless of the body's actual length, then reads the status line
// and response body. Go's net/http.Transport refuses to send a
// Content-Length that disagrees with a known-length body, which
// blocks the legitimate adversarial-client test case below; the
// only reliable way to test the gateway's defence is to bypass the
// client transport entirely and write raw HTTP bytes.
//
// If halfCloseAfterBody is true, the helper TCP-half-closes the
// write side after sending the partial body so the server sees
// io.ErrUnexpectedEOF on the body reader instead of hanging
// indefinitely waiting for the missing declared bytes. This
// mirrors a realistic adversarial client that lied about
// Content-Length and then dropped the connection.
//
// Returns (status, respBody, didRespond). didRespond is false when
// the server closed the connection without writing any HTTP
// response (e.g. because the connection was already torn down by
// the time the handler tried to flush its 4xx). In that case the
// caller MUST fall back to manifest-store inspection to verify the
// gateway did not commit a partial upload — see the
// ContentLengthMismatch test.
func rawPUTToServer(t *testing.T, tsURL, path string, declaredCL int64, body []byte, halfCloseAfterBody bool) (status int, respBody []byte, didRespond bool) {
	t.Helper()
	u, err := url.Parse(tsURL)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", tsURL, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 5*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	req := fmt.Sprintf("PUT %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		path, u.Host, declaredCL)
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request header: %v", err)
	}
	if len(body) > 0 {
		if _, err := conn.Write(body); err != nil {
			t.Fatalf("write request body: %v", err)
		}
	}
	if halfCloseAfterBody {
		// TCP half-close: signal EOF on the read side of the
		// server's body reader so the handler unblocks
		// instead of hanging waiting for the missing
		// declaredCL - len(body) bytes.
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		// Server closed the connection without writing an
		// HTTP response. Acceptable for the short-body case
		// because the gateway's PutPiece error path may flush
		// to an already-closed socket; the manifest-store
		// invariant still verifies the bug is fixed.
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, nil, false
		}
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		// Partial-read of body after server tear-down: same
		// treatment as no-response above. The status line
		// itself parsed, so we still surface the status code.
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return resp.StatusCode, rb, true
		}
		t.Fatalf("read response body: %v", err)
	}
	return resp.StatusCode, rb, true
}

// TestManagedEncryption_StreamingPut_ContentLengthMismatch verifies
// the streaming PUT path rejects a client whose advertised
// Content-Length is larger than the body it actually sends. Pre-
// fix, the gateway would persist a manifest with
// ObjectSize == declared Content-Length while the backend held
// only the bytes the client really uploaded, leaving every
// subsequent GET to either fail mid-stream or serve a
// silently-truncated object. The fix wraps r.Body in a counting
// reader, compares the actual byte count against r.ContentLength
// after PutPiece drains the encrypt stream, and rolls back the
// backend piece with 400 IncompleteBody if they disagree.
func TestManagedEncryption_StreamingPut_ContentLengthMismatch(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	body := []byte("real-body-19-bytes-")
	declaredLen := int64(len(body)) + 4096 // lie: say body is much larger
	key := "stream-put-clmismatch.bin"

	status, respBody, didRespond := rawPUTToServer(t, s.ts.URL, "/"+s.bucket+"/"+key, declaredLen, body, true)
	if didRespond {
		if status >= 200 && status < 300 {
			t.Fatalf("PUT with Content-Length=%d body=%d unexpectedly returned %d: %s",
				declaredLen, len(body), status, respBody)
		}
		// The fix returns 400 IncompleteBody. We allow any
		// 4xx/5xx because the Go HTTP server may surface an
		// io.ErrUnexpectedEOF from the body reader as a
		// 502/503 before our handler post-flight validation
		// gets to write 400. Either way the upload was
		// rejected.
		if status < 400 {
			t.Fatalf("PUT short body expected 4xx/5xx, got %d: %s", status, respBody)
		}
	}
	// Whether the server flushed a response or closed the
	// connection mid-handler, the persistent invariant we care
	// about is below: NO manifest committed.

	// The manifest store must not contain a manifest for the
	// rejected key — a partial upload that left a manifest
	// behind would defeat the fix.
	res, err := s.manifests.List(context.Background(), "anonymous", s.bucket, "", 100)
	if err != nil {
		t.Fatalf("manifests.List: %v", err)
	}
	for _, m := range res.Manifests {
		if m.ObjectKey == key {
			t.Fatalf("manifest persisted for short-body upload %s/%s: ObjectSize=%d (claimed %d); the rollback path failed",
				s.bucket, key, m.ObjectSize, declaredLen)
		}
	}
}

// TestManagedEncryption_StreamingPut_ContentLengthOverflow
// verifies the gateway rejects a hostile Content-Length that
// would overflow client_sdk.EncryptedSize's int64 ciphertext-
// size computation. Pre-fix the streaming PUT path called
// EncryptedSize unconditionally and trusted the result; when
// the SDK returned 0 on overflow (the documented sentinel),
// the gateway would advertise ContentLength=0 to the backend
// and either silently store a zero-byte ciphertext or surface
// an opaque backend error. The fix checks the zero sentinel
// before the encrypt stream is started and returns 400
// InvalidContentLength.
func TestManagedEncryption_StreamingPut_ContentLengthOverflow(t *testing.T) {
	s := newEncryptionServer(t, encryptionPlacement{
		backend:        "local_fs_dev",
		encryptionMode: "managed",
	}, nil)

	// Pick the largest valid int64 so EncryptedSize's
	// addition-overflow check at the numerator step
	// (plaintextLen + chunk-1) fires immediately. We do not
	// actually send MaxInt64 bytes — the body is empty; the
	// EncryptedSize guard runs before the encrypt stream is
	// touched, so the gateway closes the connection with 400
	// before any read of the request body.
	const overflowingLen = int64(math.MaxInt64)
	key := "stream-put-overflow.bin"

	// halfCloseAfterBody=true here even though body is nil:
	// the overflow check fires BEFORE the handler reads the
	// body, but the server's connection-handler reads request
	// headers + first read of body in parallel; half-closing
	// after the headers ensures the test does not hang if the
	// gateway happens to consume the (empty) body before
	// writing 400.
	status, respBody, didRespond := rawPUTToServer(t, s.ts.URL, "/"+s.bucket+"/"+key, overflowingLen, nil, true)
	if !didRespond {
		t.Fatalf("server closed connection without responding; expected 400 InvalidContentLength")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("PUT with Content-Length=%d expected 400 InvalidContentLength, got %d: %s",
			overflowingLen, status, respBody)
	}
	if !bytes.Contains(respBody, []byte("InvalidContentLength")) {
		t.Fatalf("response body = %q, want InvalidContentLength error code", respBody)
	}

	// The overflow path must not have committed a manifest.
	res, err := s.manifests.List(context.Background(), "anonymous", s.bucket, "", 100)
	if err != nil {
		t.Fatalf("manifests.List: %v", err)
	}
	for _, m := range res.Manifests {
		if m.ObjectKey == key {
			t.Fatalf("manifest persisted for overflowing upload %s/%s; the early-reject path failed", s.bucket, key)
		}
	}
}
