package s3_conformance_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	conformance "github.com/kennguy3n/zk-object-fabric/tests/s3_conformance"
)

// fixedPlacement resolves every object to a single backend, mirroring
// the helper in tests/s3_compat. We duplicate the small struct here
// rather than importing the test-package version because Go forbids
// cross-test-package imports.
type fixedPlacement struct {
	backend string
}

func (f fixedPlacement) ResolveBackend(string, string, string) (string, metadata.PlacementPolicy, error) {
	return f.backend, metadata.PlacementPolicy{
		AllowedBackends: []string{f.backend},
	}, nil
}

// newLocalFSGateway stands up the gateway handler in-process backed
// by the on-disk local_fs_dev provider, identical to how
// tests/s3_compat does it, and returns an SDK client + bucket name
// pointing at the httptest.Server.
func newLocalFSGateway(t *testing.T) (*s3.Client, string, string) {
	t.Helper()
	root := t.TempDir()
	provider, err := local_fs_dev.New(root)
	if err != nil {
		t.Fatalf("local_fs_dev.New: %v", err)
	}
	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests: memory.New(),
		Providers: map[string]providers.StorageProvider{"local": provider},
		Placement: fixedPlacement{backend: "local"},
		Multipart: multipart.NewMemoryStore(),
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Now:       time.Now,
	}).Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	cfg, err := config.LoadDefaultConfig(
		context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Fatalf("config.LoadDefaultConfig: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
	return client, ts.URL, "conformance-bucket"
}

// TestRunConformance_LocalFSDev is the headline conformance gate.
// It drives the full Runner battery against a gateway backed by
// local_fs_dev (the same backend the Docker demo uses), then asserts
// that:
//
//   - every operation in the matrix has a non-empty Op field
//   - every "core" / "listing" / "range" / "multipart" / "copy" op
//     either passed or was explicitly classified as Unsupported
//     (we never expect Failed or Errored on the local_fs_dev path)
//   - the intentionally-unsupported operations (acl, tagging,
//     lifecycle, bucket-versioning, bulk DeleteObjects) all returned
//     a 4xx and were correctly classified as Unsupported (not
//     silently accepted with OpFailed)
//   - the matrix serialises cleanly to both JSON and Markdown
//
// This test is the regression gate for the gateway's S3 surface:
// any new feature that breaks conformance will trip one of the
// expected-Passed operations, and any feature that *silently* drops a
// previously-unsupported op will trip the "expected 4xx" guard.
func TestRunConformance_LocalFSDev(t *testing.T) {
	client, endpoint, bucket := newLocalFSGateway(t)
	r := &conformance.Runner{
		Client:       client,
		Endpoint:     endpoint,
		Bucket:       bucket,
		CreateBucket: false, // gateway auto-creates on first PUT
		Cleanup:      true,
	}
	matrix := r.Run(context.Background())

	if len(matrix.Operations) == 0 {
		t.Fatalf("Run returned an empty matrix")
	}
	for _, op := range matrix.Operations {
		if op.Op == "" {
			t.Errorf("matrix entry has empty Op: %+v", op)
		}
		if op.Category == "" {
			t.Errorf("matrix entry %s has empty Category", op.Op)
		}
	}

	// Core surface must pass.
	mustPass := []string{
		"HeadBucket",
		"PutObject",
		"HeadObject",
		"GetObject",
		"DeleteObject",
		"DeleteObject_Idempotent",
		"GetObject_MissingKey",
		"ListObjectsV2_Prefix",
		"GetObject_RangeMiddle",
		"GetObject_RangeOpen",
		"CreateMultipartUpload",
		"UploadPart",
		"CompleteMultipartUpload",
		"CopyObject_SameBucket",
	}
	byOp := indexByOp(matrix.Operations)
	for _, op := range mustPass {
		entry, ok := byOp[op]
		if !ok {
			t.Errorf("missing matrix entry for %q", op)
			continue
		}
		if entry.Status != conformance.OpPassed {
			t.Errorf("op %q: status=%s, detail=%s — expected passed", op, entry.Status, entry.Detail)
		}
	}

	// Intentionally-unsupported ops must be classified as
	// Unsupported (not Failed). A failure here means the gateway
	// silently started accepting an operation it doesn't actually
	// honour, which is worse than the original gap because clients
	// would assume their request took effect.
	mustBeUnsupported := []string{
		"GetObjectAcl",
		"PutObjectAcl",
		"PutObjectTagging",
		"GetObjectTagging",
		"DeleteObjectTagging",
		"PutBucketLifecycleConfiguration",
		"GetBucketLifecycleConfiguration",
		"PutBucketVersioning",
		"GetBucketVersioning",
		"DeleteObjects",
	}
	for _, op := range mustBeUnsupported {
		entry, ok := byOp[op]
		if !ok {
			t.Errorf("missing matrix entry for %q", op)
			continue
		}
		// We assert the exact OpUnsupported status (not just
		// "not OpFailed") because OpErrored — the harness's
		// own error path (network teardown, marshalling
		// failure) — would otherwise pass this check silently
		// and let an infrastructure regression slip through
		// looking like a healthy "Unsupported" entry. The
		// SLA we want is positive: the gateway responded, and
		// the response was an explicit 4xx/501 we classified
		// as Unsupported.
		if entry.Status != conformance.OpUnsupported {
			t.Errorf("op %q: status=%s (expected OpUnsupported) — detail=%s",
				op, entry.Status, entry.Detail)
		}
	}

	// JSON + Markdown serialisation round-trip — exercised here so
	// the test suite covers the report writers without needing a
	// disk artifact in the repo. We also drop the matrix under
	// t.TempDir() so on failure the bytes are inspectable via
	// `go test -test.run=…` with `-v -test.outputdir=…`.
	jsonPath := filepath.Join(t.TempDir(), "matrix.json")
	jsonFile, err := os.Create(jsonPath)
	if err != nil {
		t.Fatalf("create matrix.json: %v", err)
	}
	if err := matrix.WriteJSON(jsonFile); err != nil {
		jsonFile.Close()
		t.Fatalf("WriteJSON: %v", err)
	}
	if err := jsonFile.Close(); err != nil {
		t.Fatalf("close matrix.json: %v", err)
	}
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read matrix.json: %v", err)
	}
	var roundTrip conformance.Matrix
	if err := json.Unmarshal(data, &roundTrip); err != nil {
		t.Fatalf("unmarshal matrix.json: %v", err)
	}
	if len(roundTrip.Operations) != len(matrix.Operations) {
		t.Errorf("JSON round trip dropped operations: got %d want %d", len(roundTrip.Operations), len(matrix.Operations))
	}

	mdPath := filepath.Join(t.TempDir(), "matrix.md")
	mdFile, err := os.Create(mdPath)
	if err != nil {
		t.Fatalf("create matrix.md: %v", err)
	}
	if err := matrix.WriteMarkdown(mdFile); err != nil {
		mdFile.Close()
		t.Fatalf("WriteMarkdown: %v", err)
	}
	if err := mdFile.Close(); err != nil {
		t.Fatalf("close matrix.md: %v", err)
	}
	md, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read matrix.md: %v", err)
	}
	rendered := string(md)
	for _, marker := range []string{"# S3 Conformance Matrix", "| Operation |", "PutObject", "GetObject"} {
		if !strings.Contains(rendered, marker) {
			t.Errorf("rendered markdown missing %q", marker)
		}
	}
}

// TestRunConformance_PassedSummary asserts that the matrix's
// Counts() helper correctly tallies status counts and that
// AllPassed() rejects matrices with even a single Failed entry.
// This is the gate CI uses to decide whether to publish a green
// conformance report or block the build.
func TestRunConformance_PassedSummary(t *testing.T) {
	cases := []struct {
		name     string
		ops      []conformance.OpResult
		want     map[conformance.OpStatus]int
		allPass  bool
	}{
		{
			name: "all passed",
			ops: []conformance.OpResult{
				{Op: "a", Status: conformance.OpPassed},
				{Op: "b", Status: conformance.OpPassed},
			},
			want:    map[conformance.OpStatus]int{conformance.OpPassed: 2},
			allPass: true,
		},
		{
			name: "passed plus unsupported still passes",
			ops: []conformance.OpResult{
				{Op: "a", Status: conformance.OpPassed},
				{Op: "b", Status: conformance.OpUnsupported},
			},
			want:    map[conformance.OpStatus]int{conformance.OpPassed: 1, conformance.OpUnsupported: 1},
			allPass: true,
		},
		{
			name: "one failed trips the gate",
			ops: []conformance.OpResult{
				{Op: "a", Status: conformance.OpPassed},
				{Op: "b", Status: conformance.OpFailed},
			},
			want:    map[conformance.OpStatus]int{conformance.OpPassed: 1, conformance.OpFailed: 1},
			allPass: false,
		},
		{
			name: "errored also trips the gate",
			ops: []conformance.OpResult{
				{Op: "a", Status: conformance.OpErrored},
			},
			want:    map[conformance.OpStatus]int{conformance.OpErrored: 1},
			allPass: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := conformance.Matrix{Operations: tc.ops}
			counts := m.Counts()
			for status, want := range tc.want {
				if got := counts[status]; got != want {
					t.Errorf("counts[%s] = %d, want %d", status, got, want)
				}
			}
			if got := m.AllPassed(); got != tc.allPass {
				t.Errorf("AllPassed() = %v, want %v", got, tc.allPass)
			}
		})
	}
}

func indexByOp(ops []conformance.OpResult) map[string]conformance.OpResult {
	out := make(map[string]conformance.OpResult, len(ops))
	for _, op := range ops {
		out[op.Op] = op
	}
	return out
}

// TestRunner_RunDoesNotMutateReceiver asserts the contract documented
// on Runner.Run: a second call on the same Runner instance is
// independent of the first. Concretely, when KeyPrefix is left empty
// the runner derives a fresh per-call namespace; if Run mutated the
// receiver, the second call would inherit the first call's stale
// prefix (and would also collide with the first call's cleanup pass,
// since both runs would write to and then delete the same namespace).
//
// We also assert the inverse: when KeyPrefix is set explicitly, Run
// honours it for every call and never overwrites it on the receiver
// (operator-supplied prefixes must round-trip).
func TestRunner_RunDoesNotMutateReceiver(t *testing.T) {
	client, endpoint, bucket := newLocalFSGateway(t)
	ctx := context.Background()

	// Case 1: empty KeyPrefix — two Run() calls must each generate
	// a distinct timestamped prefix without mutating the receiver.
	r := &conformance.Runner{
		Client:       client,
		Endpoint:     endpoint,
		Bucket:       bucket,
		CreateBucket: true,
		Cleanup:      true,
	}
	if r.KeyPrefix != "" {
		t.Fatalf("precondition: KeyPrefix should be empty, got %q", r.KeyPrefix)
	}
	m1 := r.Run(ctx)
	if r.KeyPrefix != "" {
		t.Errorf("Run() mutated receiver KeyPrefix to %q; expected empty (contract: Runner is reusable)", r.KeyPrefix)
	}
	// Sleep one second so the second run's timestamp differs from
	// the first — otherwise both runs would happen to pick the
	// same fresh prefix purely because they ran in the same wall
	// clock second, and the assertion below would be ambiguous.
	time.Sleep(1100 * time.Millisecond)
	m2 := r.Run(ctx)
	if r.KeyPrefix != "" {
		t.Errorf("second Run() also mutated receiver KeyPrefix to %q", r.KeyPrefix)
	}
	// Both matrices should be fully populated (no carry-over
	// failures from collision).
	if c := m1.Counts()[conformance.OpFailed] + m1.Counts()[conformance.OpErrored]; c > 0 {
		t.Errorf("first run produced %d Failed/Errored ops", c)
	}
	if c := m2.Counts()[conformance.OpFailed] + m2.Counts()[conformance.OpErrored]; c > 0 {
		t.Errorf("second run produced %d Failed/Errored ops", c)
	}

	// Case 2: caller-set KeyPrefix — Run must use it verbatim and
	// must not overwrite it.
	explicit := "fixed-prefix-test/"
	r2 := &conformance.Runner{
		Client:       client,
		Endpoint:     endpoint,
		Bucket:       bucket,
		CreateBucket: true,
		Cleanup:      true,
		KeyPrefix:    explicit,
	}
	_ = r2.Run(ctx)
	if r2.KeyPrefix != explicit {
		t.Errorf("Run() altered caller-supplied KeyPrefix: got %q want %q", r2.KeyPrefix, explicit)
	}
}
