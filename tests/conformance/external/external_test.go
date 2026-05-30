package external

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestCompactDetail_StripsNewlines(t *testing.T) {
	got := compactDetail("first\nsecond\r\nthird")
	if strings.ContainsAny(got, "\n\r") {
		t.Errorf("compactDetail still contains newlines: %q", got)
	}
}
