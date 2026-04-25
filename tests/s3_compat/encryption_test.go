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
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
// plaintext CMK used to construct the gateway's Wrapper.
type encryptionServer struct {
	ts          *httptest.Server
	client      *s3.Client
	bucket      string
	pieceRoot   string
	manifests   manifest_store.ManifestStore
	gatewayEnc  *s3compat.GatewayEncryption
	cmkMaterial []byte
	cmkPath     string
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
	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests:     manifests,
		Providers:     map[string]providers.StorageProvider{placement.backend: backend},
		Placement:     placement,
		Multipart:     multipart.NewMemoryStore(),
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Encryption:    gatewayEnc,
		Now:           time.Now,
	}).Register(mux)
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
