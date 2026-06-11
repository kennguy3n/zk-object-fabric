package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/migration"
)

// nilJobsStore is a JobStore whose Jobs() returns a nil slice, exactly
// like PgJobStore when the migration_jobs table is empty. Embedding the
// interface leaves every other method nil — list() only ever calls
// Jobs(), so that is all this fake needs to override.
type nilJobsStore struct{ migration.JobStore }

func (nilJobsStore) Jobs(context.Context) ([]migration.MigrationJob, error) {
	return nil, nil
}

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

	// The direct ServeHTTP mount (reverse proxy / chi router) must agree
	// with the mux on the nil-Orchestrator case: claim the migrations
	// subtree and 404 it rather than falling through to dispatch()'s 400.
	directSrv := httptest.NewServer(h)
	defer directSrv.Close()
	for _, path := range []string{"/api/v1/migrations", "/api/v1/migrations/j1"} {
		dResp, err := http.Get(directSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		got := dResp.StatusCode
		dResp.Body.Close()
		if got != http.StatusNotFound {
			t.Fatalf("ServeHTTP %s: status=%d, want 404 (must match the mux, not dispatch()'s 400)", path, got)
		}
	}
}

// TestMigrationHandler_AdminAuthGate verifies the fleet queue is
// operator-gated like its sibling CostHandler: a non-nil AdminAuth
// that rejects must 401 both the list and the single-job route (the
// queue carries cross-tenant tenant/cell IDs, so it must never be
// readable by a caller that fails the admin check), while an
// accepting hook passes through to 200.
func TestMigrationHandler_AdminAuthGate(t *testing.T) {
	o := migration.NewFleetOrchestrator(nil, nil)
	_ = o.Enqueue(migration.MigrationJob{JobID: "j1", TenantID: "T", DestCellID: "c"})

	for _, path := range []string{"/api/v1/migrations", "/api/v1/migrations/j1"} {
		denySrv := httptest.NewServer(&MigrationHandler{
			Orchestrator: o,
			AdminAuth:    func(*http.Request) bool { return false },
		})
		resp, err := http.Get(denySrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		gotDeny := resp.StatusCode
		resp.Body.Close()
		denySrv.Close()
		if gotDeny != http.StatusUnauthorized {
			t.Fatalf("%s denied: status=%d, want 401", path, gotDeny)
		}

		allowSrv := httptest.NewServer(&MigrationHandler{
			Orchestrator: o,
			AdminAuth:    func(*http.Request) bool { return true },
		})
		resp, err = http.Get(allowSrv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		gotAllow := resp.StatusCode
		resp.Body.Close()
		allowSrv.Close()
		if gotAllow != http.StatusOK {
			t.Fatalf("%s allowed: status=%d, want 200", path, gotAllow)
		}
	}
}

// TestMigrationHandler_EmptyListIsArrayNotNull guards the list
// endpoint's JSON contract: an empty fleet must serialize to `[]`,
// never `null`. PgJobStore.Jobs() returns a nil slice when the
// migration_jobs table is empty (its steady state), and the SPA infers
// AdminAuth gating purely from the HTTP status — a 200 with a `null`
// body would be mis-read as "Operator access required" instead of "no
// migrations". The in-memory store returns a non-nil empty slice, so a
// store that returns nil (like Postgres) is used here to exercise the
// exact production path; the nil Orchestrator case is checked too.
func TestMigrationHandler_EmptyListIsArrayNotNull(t *testing.T) {
	nilStoreOrch, err := migration.NewFleetOrchestratorWithStore(migration.FleetOrchestratorConfig{
		Store:  nilJobsStore{},
		NodeID: "test-node",
	})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]*MigrationHandler{
		"nil-slice store (Postgres empty table)": {Orchestrator: nilStoreOrch},
		"nil orchestrator":                       {Orchestrator: nil},
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		resp, err := http.Get(srv.URL + "/api/v1/migrations")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			srv.Close()
			t.Fatalf("%s: status=%d, want 200", name, resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		srv.Close()
		if got := strings.TrimSpace(string(body)); got != "[]" {
			t.Errorf("%s: body=%q, want \"[]\" (must never be null)", name, got)
		}
	}
}

// TestConsoleHandler_MigrationRouteServeHTTPMatchesRegister locks in
// the symmetry between the two mount surfaces: when an Orchestrator is
// wired, GET /api/v1/migrations[/{jobId}] must resolve identically
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
