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

// TestConsoleHandler_RegistersMigrationRoutesWhenOrchestratorWired
// asserts the production wire path: when console.Config.Orchestrator
// is non-nil, console.Handler.Register attaches the
// /api/v1/migrations routes to the supplied mux. Without this
// the cmd/gateway plumbing would compile but the management UI
// would still 404 every migration request.
func TestConsoleHandler_RegistersMigrationRoutesWhenOrchestratorWired(t *testing.T) {
	o := migration.NewFleetOrchestrator(nil, nil)
	_ = o.Enqueue(migration.MigrationJob{JobID: "j-route", TenantID: "T", DestCellID: "c"})

	h := New(Config{Orchestrator: o})
	mux := http.NewServeMux()
	h.Register(mux)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (routes not registered)", resp.StatusCode)
	}
	var got []migration.MigrationJob
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].JobID != "j-route" {
		t.Fatalf("got=%+v, want exactly j-route", got)
	}
}

// TestConsoleHandler_OmitsMigrationRoutesWhenOrchestratorNil
// asserts the nil-Orchestrator guard: deployments without a
// metadata DB (and therefore without a FleetOrchestrator) must
// not see the migration routes on the console mux, both because
// the routes would 404 anyway (Orchestrator nil → handler nil)
// and because attaching them would shadow other handlers under
// /api/v1/migrations/.
func TestConsoleHandler_OmitsMigrationRoutesWhenOrchestratorNil(t *testing.T) {
	h := New(Config{Orchestrator: nil})
	mux := http.NewServeMux()
	h.Register(mux)

	// A request to /api/v1/migrations on this mux should NOT
	// be intercepted by the migration handler — the default
	// ServeMux behaviour returns 404.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/migrations")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404 (handler should be unregistered)", resp.StatusCode)
	}
}

// TestConsoleHandler_MigrationRouteServeHTTPMatchesRegister locks in
// the symmetry between the two mount surfaces: when an Orchestrator is
// wired, GET /api/v1/migrations[/ {jobId}] must resolve identically
// whether the console handler is attached via Register (a ServeMux) or
// used directly as an http.Handler (ServeHTTP, e.g. a reverse proxy or
// chi router). The ServeHTTP path previously fell through to dispatch()
// and 400'd the list the MigrationsPage polls.
func TestConsoleHandler_MigrationRouteServeHTTPMatchesRegister(t *testing.T) {
	newHandler := func() *Handler {
		o := migration.NewFleetOrchestrator(nil, nil)
		_ = o.Enqueue(migration.MigrationJob{JobID: "j-route", TenantID: "T", DestCellID: "c"})
		return New(Config{Orchestrator: o})
	}

	muxSrv := func() *httptest.Server {
		mux := http.NewServeMux()
		newHandler().Register(mux)
		return httptest.NewServer(mux)
	}()
	defer muxSrv.Close()

	directSrv := httptest.NewServer(newHandler())
	defer directSrv.Close()

	for _, path := range []string{"/api/v1/migrations", "/api/v1/migrations/j-route"} {
		viaMux := http.StatusTeapot
		viaDirect := http.StatusTeapot
		for srv, code := range map[*httptest.Server]*int{muxSrv: &viaMux, directSrv: &viaDirect} {
			resp, err := http.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			*code = resp.StatusCode
			resp.Body.Close()
		}
		if viaMux != http.StatusOK || viaDirect != http.StatusOK {
			t.Fatalf("%s: mux=%d direct=%d, want both 200 (ServeHTTP must mirror Register)", path, viaMux, viaDirect)
		}
	}
}
