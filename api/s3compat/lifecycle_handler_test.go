package s3compat

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
)

// putLifecycle issues PUT /{bucket}?lifecycle with the given body and
// returns the recorder.
func putLifecycle(t *testing.T, h *Handler, bucket, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/"+bucket+"?lifecycle", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	return rec
}

func getLifecycle(t *testing.T, h *Handler, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/"+bucket+"?lifecycle", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	return rec
}

func deleteLifecycle(t *testing.T, h *Handler, bucket string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, "/"+bucket+"?lifecycle", nil)
	rec := httptest.NewRecorder()
	h.dispatch(rec, req)
	return rec
}

func TestGetBucketLifecycle_UnsetReturns404(t *testing.T) {
	h, _ := newVersioningTestHandler()
	rec := getLifecycle(t, h, "bucket")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?lifecycle unset = %d, want 404; body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "NoSuchLifecycleConfiguration") {
		t.Fatalf("expected NoSuchLifecycleConfiguration; got %s", rec.Body)
	}
}

func TestPutGetBucketLifecycle_RoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()

	body := `<LifecycleConfiguration>
  <Rule>
    <ID>expire-logs</ID>
    <Status>Enabled</Status>
    <Filter><Prefix>logs/</Prefix></Filter>
    <Expiration><Days>30</Days></Expiration>
  </Rule>
  <Rule>
    <ID>abort-mpu</ID>
    <Status>Enabled</Status>
    <Filter><Prefix>uploads/</Prefix></Filter>
    <AbortIncompleteMultipartUpload><DaysAfterInitiation>7</DaysAfterInitiation></AbortIncompleteMultipartUpload>
  </Rule>
</LifecycleConfiguration>`

	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?lifecycle = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	rec := getLifecycle(t, h, "bucket")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET ?lifecycle = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	var doc lifecycleConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(doc.Rules))
	}
	if doc.Rules[0].ID != "expire-logs" || doc.Rules[0].Expiration == nil || doc.Rules[0].Expiration.Days != 30 {
		t.Fatalf("rule 0 round-trip mismatch: %+v", doc.Rules[0])
	}
	if doc.Rules[0].Filter == nil || doc.Rules[0].Filter.Prefix == nil || *doc.Rules[0].Filter.Prefix != "logs/" {
		t.Fatalf("rule 0 filter mismatch: %+v", doc.Rules[0].Filter)
	}
	if doc.Rules[1].Abort == nil || doc.Rules[1].Abort.DaysAfterInitiation != 7 {
		t.Fatalf("rule 1 abort mismatch: %+v", doc.Rules[1])
	}
}

func TestPutBucketLifecycle_AndFilterRoundTrip(t *testing.T) {
	h, _ := newVersioningTestHandler()

	body := `<LifecycleConfiguration>
  <Rule>
    <ID>multi</ID>
    <Status>Enabled</Status>
    <Filter>
      <And>
        <Prefix>data/</Prefix>
        <Tag><Key>archive</Key><Value>true</Value></Tag>
        <ObjectSizeGreaterThan>1024</ObjectSizeGreaterThan>
      </And>
    </Filter>
    <Expiration><Days>90</Days></Expiration>
  </Rule>
</LifecycleConfiguration>`

	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?lifecycle And = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	rec := getLifecycle(t, h, "bucket")
	var doc lifecycleConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Rules) != 1 || doc.Rules[0].Filter == nil || doc.Rules[0].Filter.And == nil {
		t.Fatalf("expected <And> filter on round-trip; got %+v", doc.Rules[0].Filter)
	}
	and := doc.Rules[0].Filter.And
	if and.Prefix != "data/" || len(and.Tags) != 1 || and.Tags[0].Key != "archive" {
		t.Fatalf("And filter mismatch: %+v", and)
	}
	if and.ObjectSizeGreaterThan == nil || *and.ObjectSizeGreaterThan != 1024 {
		t.Fatalf("And size mismatch: %+v", and.ObjectSizeGreaterThan)
	}
}

func TestPutBucketLifecycle_Rejections(t *testing.T) {
	h, _ := newVersioningTestHandler()

	tests := []struct {
		name string
		body string
		want int
	}{
		{
			name: "malformed xml",
			body: `<LifecycleConfiguration><Rule>`,
			want: http.StatusBadRequest,
		},
		{
			name: "empty config (no rules)",
			body: `<LifecycleConfiguration></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			name: "rule with no action",
			body: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Filter><Prefix>x/</Prefix></Filter></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			name: "invalid status",
			body: `<LifecycleConfiguration><Rule><Status>Bogus</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			name: "filter and top-level prefix both set",
			body: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Prefix>a/</Prefix><Filter><Prefix>b/</Prefix></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			name: "bad date",
			body: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Date>not-a-date</Date></Expiration></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			name: "abort combined with tag filter",
			body: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Filter><Tag><Key>k</Key><Value>v</Value></Tag></Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>3</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
		{
			// Multiple predicates at the Filter root (Prefix + Tag)
			// without an <And> wrapper is MalformedXML in S3.
			name: "multi-predicate filter without And wrapper",
			body: `<LifecycleConfiguration><Rule><Status>Enabled</Status><Filter><Prefix>logs/</Prefix><Tag><Key>k</Key><Value>v</Value></Tag></Filter><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`,
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := putLifecycle(t, h, "bucket", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("PUT %q = %d, want %d; body=%s", tc.name, rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestDeleteBucketLifecycle_Idempotent(t *testing.T) {
	h, _ := newVersioningTestHandler()

	// Delete with nothing configured: 204 no-op.
	if rec := deleteLifecycle(t, h, "bucket"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?lifecycle (unset) = %d, want 204; body=%s", rec.Code, rec.Body)
	}

	// Configure, delete, confirm gone.
	body := `<LifecycleConfiguration><Rule><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?lifecycle = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	if rec := deleteLifecycle(t, h, "bucket"); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE ?lifecycle = %d, want 204; body=%s", rec.Code, rec.Body)
	}
	if rec := getLifecycle(t, h, "bucket"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET ?lifecycle after delete = %d, want 404; body=%s", rec.Code, rec.Body)
	}
}

func TestBucketLifecycle_TenantIsolation(t *testing.T) {
	h, store := newVersioningTestHandler()

	body := `<LifecycleConfiguration><Rule><ID>r</ID><Status>Enabled</Status><Expiration><Days>5</Days></Expiration></Rule></LifecycleConfiguration>`
	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT ?lifecycle = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	// The handler stores under the anonymous tenant (Auth==nil). A
	// different tenant must not see it via the store API.
	cfg, err := store.GetLifecycle(httptest.NewRequest(http.MethodGet, "/", nil).Context(), "other-tenant", "bucket")
	if err != nil {
		t.Fatalf("GetLifecycle other tenant: %v", err)
	}
	if !cfg.Empty() {
		t.Fatalf("other tenant saw config: %+v", cfg)
	}
}

// TestPutBucketLifecycle_LegacyPrefixRule covers the pre-Filter
// rule-level <Prefix> form AWS still accepts.
func TestPutBucketLifecycle_LegacyPrefixRule(t *testing.T) {
	h, _ := newVersioningTestHandler()
	body := `<LifecycleConfiguration><Rule><ID>legacy</ID><Prefix>tmp/</Prefix><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT legacy prefix = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	rec := getLifecycle(t, h, "bucket")
	var doc lifecycleConfiguration
	if err := xml.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// On GET we normalize the legacy prefix into a <Filter><Prefix>.
	if len(doc.Rules) != 1 || doc.Rules[0].Filter == nil || doc.Rules[0].Filter.Prefix == nil || *doc.Rules[0].Filter.Prefix != "tmp/" {
		t.Fatalf("legacy prefix not normalized to filter: %+v", doc.Rules[0])
	}
}

// Sanity: a Disabled rule still round-trips and is preserved.
func TestPutBucketLifecycle_DisabledRulePreserved(t *testing.T) {
	h, store := newVersioningTestHandler()
	body := `<LifecycleConfiguration><Rule><ID>off</ID><Status>Disabled</Status><Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	if rec := putLifecycle(t, h, "bucket", body); rec.Code != http.StatusOK {
		t.Fatalf("PUT disabled = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	cfg, err := store.GetLifecycle(httptest.NewRequest(http.MethodGet, "/", nil).Context(), AnonymousTenant, "bucket")
	if err != nil {
		t.Fatalf("GetLifecycle: %v", err)
	}
	if len(cfg.Rules) != 1 || cfg.Rules[0].Status != lifecycle.StatusDisabled {
		t.Fatalf("disabled rule not preserved: %+v", cfg.Rules)
	}
}
