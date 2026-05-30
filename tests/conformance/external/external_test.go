package external

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestParseS3TestsXUnit_SingleSuite(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "s3tests", "sample-xunit.xml"))
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer f.Close()

	entries, err := ParseS3TestsXUnit(f)
	if err != nil {
		t.Fatalf("ParseS3TestsXUnit: %v", err)
	}

	if len(entries) != 7 {
		t.Fatalf("entries = %d, want 7", len(entries))
	}

	want := map[string]OpStatus{
		"test_bucket_create_naming_good": OpPassed,
		"test_object_put_get_range":      OpPassed,
		"test_object_put_acl":            OpFailed,
		"test_bucket_lifecycle_put":      OpUnsupported,
		"test_website_index":             OpErrored,
		"test_multipart_upload":          OpFailed,
		"test_bucket_versioning_put":     OpUnsupported,
	}

	got := make(map[string]OpStatus, len(entries))
	for _, e := range entries {
		got[e.Op] = e.Status
		if e.Source != SourceCephS3Tests {
			t.Errorf("entry %s: source = %q, want %q", e.Op, e.Source, SourceCephS3Tests)
		}
		if e.SDK != "" {
			t.Errorf("entry %s: SDK = %q, want empty (s3-tests is python-only)", e.Op, e.SDK)
		}
	}
	for op, wantStatus := range want {
		if got[op] != wantStatus {
			t.Errorf("entry %s: status = %q, want %q", op, got[op], wantStatus)
		}
	}

	// Spot-check duration parsing (0.412s → 412ms).
	for _, e := range entries {
		if e.Op == "test_bucket_create_naming_good" && e.DurationMs != 412 {
			t.Errorf("test_bucket_create_naming_good DurationMs = %d, want 412", e.DurationMs)
		}
	}

	// Spot-check category derivation (s3tests.functional.test_s3 → test_s3).
	for _, e := range entries {
		if e.Op == "test_object_put_acl" && e.Category != "test_s3" {
			t.Errorf("test_object_put_acl Category = %q, want test_s3", e.Category)
		}
		if e.Op == "test_website_index" && e.Category != "test_s3_website" {
			t.Errorf("test_website_index Category = %q, want test_s3_website", e.Category)
		}
	}

	// Spot-check Detail on a failure includes the harness type
	// and message (so an auditor can find the failure without
	// opening the raw XUnit file).
	for _, e := range entries {
		if e.Op == "test_object_put_acl" {
			if !strings.Contains(e.Detail, "ACL not applied") {
				t.Errorf("test_object_put_acl Detail = %q, want substring 'ACL not applied'", e.Detail)
			}
		}
	}
}

func TestParseS3TestsXUnit_WrappedSuites(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "s3tests", "wrapped-xunit.xml"))
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer f.Close()
	entries, err := ParseS3TestsXUnit(f)
	if err != nil {
		t.Fatalf("ParseS3TestsXUnit: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Status != OpPassed || entries[1].Status != OpFailed {
		t.Fatalf("statuses = %q,%q, want passed,failed", entries[0].Status, entries[1].Status)
	}
}

func TestParseS3TestsXUnit_Malformed(t *testing.T) {
	_, err := ParseS3TestsXUnit(strings.NewReader("not xml"))
	if err == nil {
		t.Fatal("expected error for malformed xml, got nil")
	}
}

func TestParseMintLog(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "mint", "aws-sdk-go", "log.json"))
	if err != nil {
		t.Fatalf("open testdata: %v", err)
	}
	defer f.Close()

	entries, err := ParseMintLog(f)
	if err != nil {
		t.Fatalf("ParseMintLog: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("entries = %d, want 5", len(entries))
	}

	want := map[string]OpStatus{
		"PutObject":           OpPassed,
		"GetObject":           OpPassed,
		"PutObjectTagging":    OpFailed,
		"GetBucketVersioning": OpUnsupported,
		"ListObjectsV2":       OpPassed,
	}
	got := make(map[string]OpStatus, len(entries))
	for _, e := range entries {
		got[e.Op] = e.Status
		if e.Source != SourceMinioMint {
			t.Errorf("entry %s: source = %q, want %q", e.Op, e.Source, SourceMinioMint)
		}
		if e.SDK != "aws-sdk-go" {
			t.Errorf("entry %s: SDK = %q, want aws-sdk-go", e.Op, e.SDK)
		}
	}
	for op, wantStatus := range want {
		if got[op] != wantStatus {
			t.Errorf("entry %s: status = %q, want %q", op, got[op], wantStatus)
		}
	}

	// Verify failure Detail combines alert + message + error.
	for _, e := range entries {
		if e.Op == "PutObjectTagging" {
			for _, sub := range []string{"PutObjectTagging failed", "tagging surface not implemented", "AccessDenied"} {
				if !strings.Contains(e.Detail, sub) {
					t.Errorf("PutObjectTagging Detail = %q, want substring %q", e.Detail, sub)
				}
			}
		}
	}
}

func TestParseMintLog_UnknownStatusErrored(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "mint", "minio-py", "log.json"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	entries, err := ParseMintLog(f)
	if err != nil {
		t.Fatalf("ParseMintLog: %v", err)
	}
	var sawWeird bool
	for _, e := range entries {
		if e.Op == "sse_c_object_put" {
			sawWeird = true
			if e.Status != OpErrored {
				t.Errorf("sse_c_object_put status = %q, want errored", e.Status)
			}
			if !strings.Contains(e.Detail, "WEIRD-STATUS") {
				t.Errorf("sse_c_object_put detail = %q, want substring 'WEIRD-STATUS'", e.Detail)
			}
		}
	}
	if !sawWeird {
		t.Fatal("expected entry for sse_c_object_put")
	}
}

func TestParseMintLogDir(t *testing.T) {
	entries, err := ParseMintLogDir(filepath.Join("testdata", "mint"))
	if err != nil {
		t.Fatalf("ParseMintLogDir: %v", err)
	}
	// 5 from aws-sdk-go + 5 from minio-py.
	if len(entries) != 10 {
		t.Fatalf("entries = %d, want 10", len(entries))
	}

	// SDK attribution must be preserved.
	bySDK := make(map[string]int)
	for _, e := range entries {
		bySDK[e.SDK]++
	}
	if bySDK["aws-sdk-go"] != 5 || bySDK["minio-py"] != 5 {
		t.Errorf("SDK counts = %v, want aws-sdk-go=5 minio-py=5", bySDK)
	}
}

// Regression: mint's entrypoint script writes BOTH a per-SDK
// log.json (under {sdk}/log.json) AND an aggregated log.json at
// the top of the bind-mount target. ParseMintLogDir must skip the
// top-level aggregated file to avoid double-counting every entry.
// See https://github.com/minio/mint/blob/master/mint.sh — the
// `cat "$test_log_file" >>"$BASE_LOG_DIR/$LOG_FILE"` line is what
// produces the aggregated copy.
func TestParseMintLogDir_SkipsAggregatedTopLevelLog(t *testing.T) {
	root := t.TempDir()
	perSDK := []byte(`{"name":"aws-sdk-go","function":"PutObject","duration":12,"status":"PASS"}` + "\n" +
		`{"name":"aws-sdk-go","function":"GetObject","duration":8,"status":"PASS"}` + "\n")
	if err := os.MkdirAll(filepath.Join(root, "aws-sdk-go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "aws-sdk-go", "log.json"), perSDK, 0o644); err != nil {
		t.Fatalf("write per-sdk: %v", err)
	}
	// Aggregated top-level log mint produces: same content as
	// the per-SDK file (just `cat`ed). If we parse it too, we'd
	// double-count.
	if err := os.WriteFile(filepath.Join(root, "log.json"), perSDK, 0o644); err != nil {
		t.Fatalf("write aggregated: %v", err)
	}
	entries, err := ParseMintLogDir(root)
	if err != nil {
		t.Fatalf("ParseMintLogDir: %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("entries = %d, want 2 (aggregated top-level log must be skipped)", len(entries))
	}
}

// Regression: ParseMintLogDir must reject log.json files deeper
// than one subdir below the walk root. The doc comment promises
// "exactly one subdirectory deep" so the implementation must match.
// A stray nested log.json (e.g. from an extracted tarball or a
// fixture inside an SDK's test data) must not be counted.
func TestParseMintLogDir_RejectsDeeperThanOneSubdir(t *testing.T) {
	root := t.TempDir()
	// Legitimate: {root}/aws-sdk-go/log.json - SHOULD be parsed.
	if err := os.MkdirAll(filepath.Join(root, "aws-sdk-go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	perSDK := []byte(`{"name":"aws-sdk-go","function":"PutObject","duration":12,"status":"PASS"}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "aws-sdk-go", "log.json"), perSDK, 0o644); err != nil {
		t.Fatalf("write per-sdk: %v", err)
	}
	// Stray nested log.json - SHOULD be ignored.
	if err := os.MkdirAll(filepath.Join(root, "aws-sdk-go", "fixtures", "stray"), 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	stray := []byte(`{"name":"aws-sdk-go","function":"StrayTest","duration":1,"status":"PASS"}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "aws-sdk-go", "fixtures", "stray", "log.json"), stray, 0o644); err != nil {
		t.Fatalf("write stray: %v", err)
	}
	entries, err := ParseMintLogDir(root)
	if err != nil {
		t.Fatalf("ParseMintLogDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (deeper-than-one-subdir log.json must be ignored)", len(entries))
	}
	if len(entries) == 1 && entries[0].Op != "PutObject" {
		t.Errorf("entry[0].Op = %q, want PutObject (stray nested log leaked in)", entries[0].Op)
	}
}

// Regression: trailing-slash root path must work (operators often
// pass paths with trailing slashes). The top-level aggregated log
// skip relies on filepath.Dir(path) == cleanRoot, which only holds
// if we canonicalise the input via filepath.Clean.
func TestParseMintLogDir_RootWithTrailingSlash(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "aws-sdk-go"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	perSDK := []byte(`{"name":"aws-sdk-go","function":"PutObject","duration":12,"status":"PASS"}` + "\n")
	if err := os.WriteFile(filepath.Join(root, "aws-sdk-go", "log.json"), perSDK, 0o644); err != nil {
		t.Fatalf("write per-sdk: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "log.json"), perSDK, 0o644); err != nil {
		t.Fatalf("write aggregated: %v", err)
	}
	entries, err := ParseMintLogDir(root + string(filepath.Separator))
	if err != nil {
		t.Fatalf("ParseMintLogDir with trailing slash: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("entries = %d, want 1 (trailing slash on root must not defeat aggregated-log skip)", len(entries))
	}
}

func TestAggregate_DeterministicJSON(t *testing.T) {
	t0 := time.Date(2026, 5, 30, 14, 0, 0, 0, time.UTC)
	parts1 := []MatrixEntry{
		{Op: "PutObject", Category: "aws-sdk-go", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
		{Op: "test_object_put_acl", Category: "test_s3", Source: SourceCephS3Tests, Status: OpFailed, Detail: "ACL not applied"},
	}
	parts2 := []MatrixEntry{
		{Op: "GetObject", Category: "aws-sdk-go", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
	}
	m := Aggregate("https://gw.example.com", "abc123", t0, parts1, parts2)

	if m.GatewayEndpoint != "https://gw.example.com" {
		t.Errorf("GatewayEndpoint = %q", m.GatewayEndpoint)
	}
	if m.GatewaySHA != "abc123" {
		t.Errorf("GatewaySHA = %q", m.GatewaySHA)
	}
	if !m.GeneratedAt.Equal(t0) {
		t.Errorf("GeneratedAt = %v, want %v", m.GeneratedAt, t0)
	}

	var b1, b2 bytes.Buffer
	if err := m.WriteJSON(&b1); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	// Mutate the slice order between writes; sort inside
	// WriteJSON should make the output identical.
	m.Entries = []MatrixEntry{
		{Op: "test_object_put_acl", Category: "test_s3", Source: SourceCephS3Tests, Status: OpFailed, Detail: "ACL not applied"},
		{Op: "GetObject", Category: "aws-sdk-go", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
		{Op: "PutObject", Category: "aws-sdk-go", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
	}
	if err := m.WriteJSON(&b2); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if b1.String() != b2.String() {
		t.Errorf("WriteJSON not deterministic across permutations:\n--- run1 ---\n%s\n--- run2 ---\n%s", b1.String(), b2.String())
	}

	// Round-trip the JSON to ensure the field shape matches our
	// declared struct (a quick guard against silent JSON tag drift).
	var rt Matrix
	if err := json.Unmarshal(b1.Bytes(), &rt); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if len(rt.Entries) != len(m.Entries) {
		t.Errorf("round-trip entry count = %d, want %d", len(rt.Entries), len(m.Entries))
	}
}

func TestMatrix_Counts_AllPassed(t *testing.T) {
	m := Matrix{Entries: []MatrixEntry{
		{Op: "a", Status: OpPassed},
		{Op: "b", Status: OpPassed},
		{Op: "c", Status: OpUnsupported},
		{Op: "d", Status: OpUnsupported},
		{Op: "e", Status: OpFailed},
	}}
	c := m.Counts()
	if c.Passed != 2 || c.Failed != 1 || c.Unsupported != 2 || c.Errored != 0 || c.Total != 5 {
		t.Errorf("counts = %+v", c)
	}
	if m.AllPassed() {
		t.Errorf("AllPassed = true; want false (1 failure)")
	}

	m2 := Matrix{Entries: []MatrixEntry{
		{Op: "a", Status: OpPassed},
		{Op: "b", Status: OpUnsupported},
	}}
	if !m2.AllPassed() {
		t.Errorf("AllPassed = false; want true (unsupported is acceptable)")
	}
}

// Regression test: an empty matrix must NOT report AllPassed. An
// operator who points the aggregator at an empty directory must
// not receive a silent audit-pass.
func TestMatrix_AllPassed_EmptyMatrixIsNotPass(t *testing.T) {
	empty := Matrix{}
	if empty.AllPassed() {
		t.Errorf("AllPassed() = true on empty matrix; want false (no entries means no evidence of pass)")
	}
	nilEntries := Matrix{Entries: nil}
	if nilEntries.AllPassed() {
		t.Errorf("AllPassed() = true on nil-entries matrix; want false")
	}
	zero := Matrix{Entries: []MatrixEntry{}}
	if zero.AllPassed() {
		t.Errorf("AllPassed() = true on zero-length-entries matrix; want false")
	}
	// Boundary: a single passing entry must pass.
	one := Matrix{Entries: []MatrixEntry{{Op: "a", Status: OpPassed}}}
	if !one.AllPassed() {
		t.Errorf("AllPassed() = false on single-pass matrix; want true")
	}
}

// Regression test: WriteJSON must NOT mutate the caller's Entries
// slice. The value receiver in Go conventionally signals no
// mutation, but sort.SliceStable on m.Entries would silently sort
// the underlying array shared with the caller.
func TestMatrix_WriteJSON_DoesNotMutateCallerEntries(t *testing.T) {
	original := []MatrixEntry{
		{Op: "PutObject", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
		{Op: "test_object_put_acl", Source: SourceCephS3Tests, Status: OpFailed, Detail: "ACL not applied"},
		{Op: "GetObject", Source: SourceMinioMint, SDK: "aws-sdk-go", Status: OpPassed},
	}
	snapshot := slicesCloneEntries(original)
	m := Matrix{Entries: original}
	if err := m.WriteJSON(&bytes.Buffer{}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if len(original) != len(snapshot) {
		t.Fatalf("length changed: %d → %d", len(snapshot), len(original))
	}
	for i := range original {
		if original[i] != snapshot[i] {
			t.Errorf("original[%d] mutated by WriteJSON: %+v → %+v", i, snapshot[i], original[i])
		}
	}
}

func slicesCloneEntries(in []MatrixEntry) []MatrixEntry {
	out := make([]MatrixEntry, len(in))
	copy(out, in)
	return out
}

func TestCompactDetail_Truncates(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := compactDetail("alert", "msg", long)
	if len(got) != 240 {
		t.Errorf("compactDetail truncation length = %d, want 240", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("compactDetail truncation does not end with '...': %q", got[len(got)-10:])
	}
}

// Regression: compactDetail must back off to a valid UTF-8 rune
// boundary when truncating. A naive [:237] slice can split a
// multi-byte rune (e.g. when a harness emits a localised error
// message or a Unicode file path).
func TestCompactDetail_TruncatesAtRuneBoundary(t *testing.T) {
	// Build a string where byte 237 lands inside a multi-byte
	// rune. "你好" is 6 bytes (3 each), so a prefix of 235 ASCII
	// bytes + "你好" places the second multi-byte rune across
	// byte 238-240. The naive slice [:237] would land inside
	// the second rune.
	head := strings.Repeat("a", 235)
	input := head + "你好" + strings.Repeat("b", 200)
	got := compactDetail(input)
	if !utf8.ValidString(got) {
		t.Errorf("compactDetail produced invalid UTF-8 for input with multi-byte rune at truncation boundary; got bytes: %x", []byte(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("compactDetail truncation does not end with '...': %q", got)
	}
	if len(got) > 240 {
		t.Errorf("compactDetail length = %d, expected ≤ 240", len(got))
	}
	// Round-trip through encoding/json to confirm the JSON
	// encoder doesn't have to escape any invalid bytes.
	b, err := jsonMarshal(struct{ D string }{D: got})
	if err != nil {
		t.Errorf("json.Marshal failed on truncated string: %v", err)
	}
	if strings.Contains(string(b), `\ufffd`) {
		t.Errorf("json.Marshal produced replacement char in output, indicating invalid UTF-8: %s", b)
	}
}

func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestCompactDetail_StripsNewlines(t *testing.T) {
	got := compactDetail("first\nsecond\r\nthird")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("compactDetail still contains newlines: %q", got)
	}
}
