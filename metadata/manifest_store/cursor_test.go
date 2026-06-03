package manifest_store

import "testing"

// TestScanCursorRoundTrip pins that the scan cursor codec is lossless
// for keys containing the characters object keys and hashes can hold —
// including the '/' a delimiter-joined cursor would have choked on.
func TestScanCursorRoundTrip(t *testing.T) {
	cases := []ManifestKey{
		{},
		{TenantID: "t1", Bucket: "b1", ObjectKeyHash: "h1", VersionID: "v1"},
		{TenantID: "tenant/with/slash", Bucket: "b", ObjectKeyHash: "a|b|c", VersionID: "v 1"},
		{TenantID: "🛰", Bucket: "", ObjectKeyHash: "", VersionID: ""},
	}
	for _, want := range cases {
		got, err := DecodeScanCursor(EncodeScanCursor(want))
		if err != nil {
			t.Fatalf("decode(encode(%v)): %v", want, err)
		}
		if got != want {
			t.Fatalf("round-trip mismatch: got %v want %v", got, want)
		}
	}
}

// TestDecodeScanCursor_Empty maps the empty cursor to the zero key so
// the first page starts from the beginning of the keyset.
func TestDecodeScanCursor_Empty(t *testing.T) {
	got, err := DecodeScanCursor("")
	if err != nil {
		t.Fatalf("decode(\"\"): %v", err)
	}
	if (got != ManifestKey{}) {
		t.Fatalf("empty cursor decoded to %v, want zero key", got)
	}
}

// TestDecodeScanCursor_Malformed rejects a non-base64 token rather than
// silently restarting the scan.
func TestDecodeScanCursor_Malformed(t *testing.T) {
	if _, err := DecodeScanCursor("$$$"); err == nil {
		t.Fatal("malformed cursor: want error, got nil")
	}
}
