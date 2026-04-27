package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/billing"
)

type fakeQuery struct{ samples []billing.UsageSample }

func (f *fakeQuery) StorageHistory(_ context.Context, _ string) ([]billing.UsageSample, error) {
	return f.samples, nil
}

func TestForecastHandler_ReturnsResultForKnownCell(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const gb = uint64(1) << 30
	q := &fakeQuery{samples: []billing.UsageSample{
		{Timestamp: now.Add(-2 * 24 * time.Hour), StorageBytes: 100 * gb},
		{Timestamp: now.Add(-1 * 24 * time.Hour), StorageBytes: 200 * gb},
		{Timestamp: now, StorageBytes: 300 * gb},
	}}
	h := &ForecastHandler{
		Forecaster: &billing.Forecaster{Query: q, Clock: func() time.Time { return now }},
		CapacityResolver: func(_ context.Context, cellID string) (uint64, bool, error) {
			if cellID != "cell-1" {
				return 0, false, nil
			}
			return 1024 * gb, true, nil
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/cells/cell-1/forecast")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got billing.ForecastResult
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.CellID != "cell-1" || got.SampleCount != 3 {
		t.Errorf("unexpected result: %+v", got)
	}
}

func TestForecastHandler_ReturnsNotFoundForUnknownCell(t *testing.T) {
	h := &ForecastHandler{
		Forecaster: &billing.Forecaster{Query: &fakeQuery{}},
		CapacityResolver: func(_ context.Context, _ string) (uint64, bool, error) {
			return 0, false, nil
		},
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/cells/missing/forecast")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestForecastHandler_RejectsNonGet(t *testing.T) {
	h := &ForecastHandler{}
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/cells/x/forecast", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
