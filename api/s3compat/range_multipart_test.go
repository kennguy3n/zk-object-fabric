package s3compat

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	stdmultipart "mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// byteRangePart is one decoded MIME part of a multipart/byteranges body.
type byteRangePart struct {
	contentType  string
	contentRange string
	body         []byte
}

// parseMultipartByteRanges validates that the recorder holds a
// well-formed multipart/byteranges response — the Content-Type carries a
// boundary, the Content-Length matches the emitted body exactly, and the
// body decodes cleanly with the stdlib MIME reader — and returns the
// decoded parts for per-range assertions.
func parseMultipartByteRanges(t *testing.T, rec *httptest.ResponseRecorder) []byteRangePart {
	t.Helper()
	ct := rec.Header().Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse Content-Type %q: %v", ct, err)
	}
	if mediaType != "multipart/byteranges" {
		t.Fatalf("media type = %q, want multipart/byteranges", mediaType)
	}
	boundary := params["boundary"]
	if boundary == "" {
		t.Fatalf("Content-Type %q carries no boundary", ct)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(rec.Body.Len()) {
		t.Fatalf("Content-Length = %q, want %d (actual body length)", got, rec.Body.Len())
	}

	mr := stdmultipart.NewReader(bytes.NewReader(rec.Body.Bytes()), boundary)
	var parts []byteRangePart
	for {
		p, perr := mr.NextPart()
		if perr == io.EOF {
			break
		}
		if perr != nil {
			t.Fatalf("decode multipart part: %v", perr)
		}
		data, rerr := io.ReadAll(p)
		if rerr != nil {
			t.Fatalf("read multipart part body: %v", rerr)
		}
		parts = append(parts, byteRangePart{
			contentType:  p.Header.Get("Content-Type"),
			contentRange: p.Header.Get("Content-Range"),
			body:         data,
		})
		_ = p.Close()
	}
	return parts
}

// assertByteRangeParts checks the decoded parts against the expected
// ranges sliced from full, including per-part Content-Type and
// Content-Range headers.
func assertByteRangeParts(t *testing.T, parts []byteRangePart, full []byte, wantContentType string, ranges [][2]int) {
	t.Helper()
	if len(parts) != len(ranges) {
		t.Fatalf("got %d parts, want %d", len(parts), len(ranges))
	}
	total := len(full)
	for i, rng := range ranges {
		start, end := rng[0], rng[1]
		wantRange := fmt.Sprintf("bytes %d-%d/%d", start, end, total)
		if parts[i].contentRange != wantRange {
			t.Errorf("part %d Content-Range = %q, want %q", i, parts[i].contentRange, wantRange)
		}
		if parts[i].contentType != wantContentType {
			t.Errorf("part %d Content-Type = %q, want %q", i, parts[i].contentType, wantContentType)
		}
		if !bytes.Equal(parts[i].body, full[start:end+1]) {
			t.Errorf("part %d body = %q, want %q", i, parts[i].body, full[start:end+1])
		}
	}
}

// TestGet_MultiRange_SinglePiece pins the multipart/byteranges response
// for a plaintext single-piece object: two disjoint ranges come back as
// a 206 with one MIME part each, the object's Content-Type on every
// part, and byte-correct slices. Single-range and whole-object behavior
// is unchanged (covered by TestGet_RangeRequest); this is the
// MinIO/Ceph/nginx multi-range shape.
func TestGet_MultiRange_SinglePiece(t *testing.T) {
	h, _, bill, _ := newTestHandler()
	body := []byte("0123456789abcdefghij")

	put := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	put.Header.Set("Content-Type", "text/plain")
	put.ContentLength = int64(len(body))
	if rec := httptest.NewRecorder(); true {
		h.Put(rec, put)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	req.Header.Set("Range", "bytes=0-3,6-9,15-19")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("multi-range GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	parts := parseMultipartByteRanges(t, rec)
	assertByteRangeParts(t, parts, body, "text/plain", [][2]int{{0, 3}, {6, 9}, {15, 19}})

	if bill.count(billing.GetRequests) != 1 {
		t.Errorf("get_requests = %d, want 1", bill.count(billing.GetRequests))
	}
	if bill.count(billing.EgressBytes) == 0 {
		t.Error("egress_bytes not emitted on multi-range GET")
	}
}

// TestGet_MultiRange_Managed exercises the buffered gateway-decrypt path:
// a managed-encryption object served as multipart/byteranges must decrypt
// to the correct plaintext slices.
func TestGet_MultiRange_Managed(t *testing.T) {
	store := memory.New()
	now := time.Unix(1700000000, 0)
	h := New(Config{
		Manifests:  store,
		Providers:  map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:  encPlacement{backend: "test", mode: string(encryption.ManagedEncrypted)},
		Billing:    &recordingBilling{},
		Encryption: newTestKeyring(t),
		Now: func() time.Time {
			cur := now
			now = now.Add(time.Second)
			return cur
		},
	})

	body := []byte("the quick brown fox jumps over the lazy dog!!")
	put := httptest.NewRequest(http.MethodPut, "/bucket/enc", bytes.NewReader(body))
	put.ContentLength = int64(len(body))
	putRec := httptest.NewRecorder()
	h.Put(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("managed PUT status = %d, want 200; body=%s", putRec.Code, putRec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/enc", nil)
	req.Header.Set("Range", "bytes=4-8,16-18")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("managed multi-range GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	parts := parseMultipartByteRanges(t, rec)
	// PUT carried no Content-Type, so the object defaults to the S3
	// binary/octet-stream Content-Type on read.
	assertByteRangeParts(t, parts, body, defaultContentType, [][2]int{{4, 8}, {16, 18}})
}

// TestGet_MultiRange_ErasureCoded exercises the EC reconstruction path:
// the object is decoded whole, then sliced into the requested ranges.
func TestGet_MultiRange_ErasureCoded(t *testing.T) {
	store := memory.New()
	h := New(Config{
		Manifests:     store,
		Providers:     map[string]providers.StorageProvider{"test": newFakeProvider("test")},
		Placement:     ecPlacement{backend: "test", profile: erasure_coding.Profile6Plus2.Name},
		ErasureCoding: erasure_coding.DefaultRegistry(),
		Now:           func() time.Time { return time.Unix(1700000000, 0) },
	})

	body := bytes.Repeat([]byte("ec-payload!"), 4096)
	put := httptest.NewRequest(http.MethodPut, "/bucket/ec-obj", bytes.NewReader(body))
	put.Header.Set("Content-Type", "application/x-ec")
	put.ContentLength = int64(len(body))
	putRec := httptest.NewRecorder()
	h.Put(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("EC PUT status = %d, want 200; body=%s", putRec.Code, putRec.Body)
	}

	req := httptest.NewRequest(http.MethodGet, "/bucket/ec-obj", nil)
	req.Header.Set("Range", "bytes=100-199,5000-5099")
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("EC multi-range GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	parts := parseMultipartByteRanges(t, rec)
	assertByteRangeParts(t, parts, body, "application/x-ec", [][2]int{{100, 199}, {5000, 5099}})
}

// TestGet_MultiRange_Multipart exercises the multipart-assembly path with
// ranges that straddle the part boundary.
func TestGet_MultiRange_Multipart(t *testing.T) {
	h, _ := newReconstructCopyHandler(t, fixedPlacement{backend: "test"}, nil)
	parts := [][]byte{
		bytes.Repeat([]byte("part-1-"), 1024),
		bytes.Repeat([]byte("part-2-"), 1024),
	}
	seedMultipartSource(t, h, parts, nil)

	full := append(append([]byte{}, parts[0]...), parts[1]...)
	boundary := len(parts[0])

	req := httptest.NewRequest(http.MethodGet, "/bucket/src", nil)
	req.Header.Set("Range", fmt.Sprintf("bytes=10-19,%d-%d", boundary-5, boundary+4))
	rec := httptest.NewRecorder()
	h.Get(rec, req)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("multipart multi-range GET status = %d, want 206; body=%s", rec.Code, rec.Body)
	}
	decoded := parseMultipartByteRanges(t, rec)
	assertByteRangeParts(t, decoded, full, defaultContentType, [][2]int{{10, 19}, {boundary - 5, boundary + 4}})
}

// TestGet_MultiRange_DegradesToWhole pins the response-amplification
// guards: a multi-range request whose combined span meets/exceeds the
// object, or that names more ranges than maxObjectRanges, is served as
// the whole object (200) rather than a multipart body — matching
// Apache's default MaxRanges degradation.
func TestGet_MultiRange_DegradesToWhole(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	put := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	put.ContentLength = int64(len(body))
	if rec := httptest.NewRecorder(); true {
		h.Put(rec, put)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
		}
	}

	t.Run("combined span >= object size", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
		// 0-5 (6 bytes) + 4-9 (6 bytes) = 12 >= 10.
		req.Header.Set("Range", "bytes=0-5,4-9")
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (whole object); body=%s", rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("body = %q, want whole object %q", rec.Body.Bytes(), body)
		}
		if got := rec.Header().Get("Content-Range"); got != "" {
			t.Errorf("Content-Range = %q, want empty on whole-object 200", got)
		}
	})

	t.Run("more ranges than the cap", func(t *testing.T) {
		segs := make([]string, 0, maxObjectRanges+1)
		for i := 0; i <= maxObjectRanges; i++ {
			segs = append(segs, "0-0")
		}
		req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
		req.Header.Set("Range", "bytes="+strings.Join(segs, ","))
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (over range cap); body=%s", rec.Code, rec.Body)
		}
		if !bytes.Equal(rec.Body.Bytes(), body) {
			t.Errorf("body = %q, want whole object %q", rec.Body.Bytes(), body)
		}
	})
}

// TestGet_MultiRange_Malformed_Returns416 pins that an unsatisfiable
// segment anywhere in a multi-range header fails the whole request with
// 416, matching the single-range parser's strictness.
func TestGet_MultiRange_Malformed_Returns416(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789")
	put := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	put.ContentLength = int64(len(body))
	if rec := httptest.NewRecorder(); true {
		h.Put(rec, put)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
		}
	}

	for _, hdr := range []string{"bytes=0-3,5-2", "bytes=0-3,50-60"} {
		req := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
		req.Header.Set("Range", hdr)
		rec := httptest.NewRecorder()
		h.Get(rec, req)
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("Range %q: status = %d, want 416; body=%s", hdr, rec.Code, rec.Body)
		}
	}
}

// TestHead_MultiRange_MirrorsGet pins RFC 9110 §13.1 for multi-range
// HEAD: the response advertises the same multipart/byteranges
// Content-Type and analytic Content-Length that the matching GET emits,
// with no message body.
func TestHead_MultiRange_MirrorsGet(t *testing.T) {
	h, _, _, _ := newTestHandler()
	body := []byte("0123456789abcdefghij")
	put := httptest.NewRequest(http.MethodPut, "/bucket/obj", bytes.NewReader(body))
	put.Header.Set("Content-Type", "text/plain")
	put.ContentLength = int64(len(body))
	if rec := httptest.NewRecorder(); true {
		h.Put(rec, put)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
		}
	}

	const rangeHdr = "bytes=0-3,6-9,15-19"

	getReq := httptest.NewRequest(http.MethodGet, "/bucket/obj", nil)
	getReq.Header.Set("Range", rangeHdr)
	getRec := httptest.NewRecorder()
	h.Get(getRec, getReq)
	if getRec.Code != http.StatusPartialContent {
		t.Fatalf("GET status = %d, want 206; body=%s", getRec.Code, getRec.Body)
	}

	headReq := httptest.NewRequest(http.MethodHead, "/bucket/obj", nil)
	headReq.Header.Set("Range", rangeHdr)
	headRec := httptest.NewRecorder()
	h.Head(headRec, headReq)
	if headRec.Code != http.StatusPartialContent {
		t.Fatalf("HEAD status = %d, want 206; body=%s", headRec.Code, headRec.Body)
	}
	if headRec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", headRec.Body.Len())
	}

	gct, _, err := mime.ParseMediaType(getRec.Header().Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse GET Content-Type: %v", err)
	}
	hct, _, err := mime.ParseMediaType(headRec.Header().Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse HEAD Content-Type: %v", err)
	}
	if gct != "multipart/byteranges" || hct != "multipart/byteranges" {
		t.Fatalf("media types = GET %q / HEAD %q, want multipart/byteranges", gct, hct)
	}
	// The boundary differs per response, but the analytic length must
	// match the bytes GET actually streamed.
	if got, want := headRec.Header().Get("Content-Length"), strconv.Itoa(getRec.Body.Len()); got != want {
		t.Errorf("HEAD Content-Length = %q, want %q (GET body length)", got, want)
	}
}
