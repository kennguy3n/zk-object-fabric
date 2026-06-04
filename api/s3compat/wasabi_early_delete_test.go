package s3compat

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	"github.com/kennguy3n/zk-object-fabric/providers"
)

// fakeWasabiProvider is a fakeProvider that identifies itself as a
// Wasabi backend via PlacementLabels, so the DELETE path's
// early-delete warning logic treats its pieces as Wasabi-resident.
type fakeWasabiProvider struct{ *fakeProvider }

func (f *fakeWasabiProvider) PlacementLabels() providers.PlacementLabels {
	return providers.PlacementLabels{Provider: "wasabi", Region: "ap-southeast-1", Country: "SG"}
}

// newWasabiClockHandler builds a handler whose single backend is a
// Wasabi-labelled provider and whose clock is driven by *clk, so a
// test can PUT an object and then advance the clock before DELETE.
func newWasabiClockHandler(clk *time.Time) (*Handler, manifest_store.ManifestStore) {
	store := memory.New()
	fake := &fakeWasabiProvider{newFakeProvider("wasabi-ap-southeast-1")}
	h := New(Config{
		Manifests: store,
		Providers: map[string]providers.StorageProvider{"wasabi-ap-southeast-1": fake},
		Placement: fixedPlacement{backend: "wasabi-ap-southeast-1"},
		Billing:   &recordingBilling{},
		Now:       func() time.Time { return *clk },
	})
	return h, store
}

func putThenDelete(t *testing.T, h *Handler, clk *time.Time, advance time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	body := []byte("payload")
	req := httptest.NewRequest(http.MethodPut, "/bkt/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	*clk = clk.Add(advance)

	req = httptest.NewRequest(http.MethodDelete, "/bkt/obj", nil)
	rec = httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204 (warn-only must never block); body=%s", rec.Code, rec.Body)
	}
	return rec
}

func TestDelete_WasabiEarlyDeleteWarning(t *testing.T) {
	day := 24 * time.Hour
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name          string
		advance       time.Duration
		wantWarn      bool
		wantRemaining string
	}{
		{"fresh object, full window", 0, true, "90"},
		{"mid window", 10 * day, true, "80"},
		{"partial day rounds up", 89*day + 12*time.Hour, true, "1"},
		{"boundary: exactly 90 days, no warning", 90 * day, false, ""},
		{"past window, no warning", 120 * day, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clk := base
			h, _ := newWasabiClockHandler(&clk)
			rec := putThenDelete(t, h, &clk, tc.advance)

			gotWarn := rec.Header().Get("X-Zkof-Wasabi-Early-Delete-Warning")
			gotDays := rec.Header().Get("X-Zkof-Wasabi-Min-Storage-Remaining-Days")
			if tc.wantWarn {
				if gotWarn != "true" {
					t.Errorf("X-Zkof-Wasabi-Early-Delete-Warning = %q, want \"true\"", gotWarn)
				}
				if gotDays != tc.wantRemaining {
					t.Errorf("X-Zkof-Wasabi-Min-Storage-Remaining-Days = %q, want %q", gotDays, tc.wantRemaining)
				}
			} else {
				if gotWarn != "" {
					t.Errorf("X-Zkof-Wasabi-Early-Delete-Warning = %q, want absent", gotWarn)
				}
				if gotDays != "" {
					t.Errorf("X-Zkof-Wasabi-Min-Storage-Remaining-Days = %q, want absent", gotDays)
				}
			}
		})
	}
}

// TestDelete_NonWasabiNoWarning confirms the headers are Wasabi-only:
// a fresh object on a non-Wasabi backend never gets the warning.
func TestDelete_NonWasabiNoWarning(t *testing.T) {
	h, _, _, _ := newTestHandler() // backend "test", PlacementLabels{} (not wasabi)
	body := []byte("payload")

	req := httptest.NewRequest(http.MethodPut, "/bkt/obj", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	rec := httptest.NewRecorder()
	h.Put(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200", rec.Code)
	}

	req = httptest.NewRequest(http.MethodDelete, "/bkt/obj", nil)
	rec = httptest.NewRecorder()
	h.Delete(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", rec.Code)
	}
	if got := rec.Header().Get("X-Zkof-Wasabi-Early-Delete-Warning"); got != "" {
		t.Errorf("non-Wasabi backend got warning header %q, want absent", got)
	}
}
