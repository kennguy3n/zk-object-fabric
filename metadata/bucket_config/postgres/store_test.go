package postgres

import "testing"

func TestNew_RejectsInvalidTable(t *testing.T) {
	if _, err := New(Config{DB: nil}); err == nil {
		t.Fatal("New(nil DB) = nil error, want error")
	}
}

func TestIsSafeIdent(t *testing.T) {
	cases := map[string]bool{
		"bucket_versioning": true,
		"BucketVersioning":  true,
		"t1":                true,
		"":                  false,
		"1table":            false,
		"bad name":          false,
		"drop;table":        false,
		"a-b":               false,
	}
	for in, want := range cases {
		if got := isSafeIdent(in); got != want {
			t.Errorf("isSafeIdent(%q) = %v, want %v", in, got, want)
		}
	}
}
