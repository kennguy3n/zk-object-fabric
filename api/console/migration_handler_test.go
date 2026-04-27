package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/migration"
)

func TestMigrationHandler_ListsJobs(t *testing.T) {
	o := migration.NewFleetOrchestrator(nil, nil)
	_ = o.Enqueue(migration.MigrationJob{JobID: "j1", TenantID: "T", DestCellID: "c"})
	_ = o.Enqueue(migration.MigrationJob{JobID: "j2", TenantID: "T", DestCellID: "c"})

	srv := httptest.NewServer(&MigrationHandler{Orchestrator: o})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got []migration.MigrationJob
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("len=%d, want 2", len(got))
	}
}

func TestMigrationHandler_SingleJob(t *testing.T) {
	o := migration.NewFleetOrchestrator(nil, nil)
	_ = o.Enqueue(migration.MigrationJob{JobID: "j1", TenantID: "T", DestCellID: "c"})
	srv := httptest.NewServer(&MigrationHandler{Orchestrator: o})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/migrations/j1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var got migration.MigrationJob
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got.JobID != "j1" {
		t.Errorf("got %+v", got)
	}
}

func TestMigrationHandler_UnknownJobIs404(t *testing.T) {
	o := migration.NewFleetOrchestrator(nil, nil)
	srv := httptest.NewServer(&MigrationHandler{Orchestrator: o})
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/v1/migrations/missing")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status=%d, want 404", resp.StatusCode)
	}
}
