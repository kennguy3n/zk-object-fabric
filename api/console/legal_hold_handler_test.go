package console

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/auth"
)

func TestLegalHoldHandler_IssueListRelease(t *testing.T) {
	store := auth.NewMemoryLegalHoldStore()
	h := &LegalHoldHandler{Store: store}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Issue
	body, _ := json.Marshal(CreateRequest{Bucket: "b", ObjectKey: "k", Reason: "case-42", IssuedBy: "ops@x"})
	resp, err := http.Post(srv.URL+"/api/v1/tenants/T/legal-hold", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("issue status=%d", resp.StatusCode)
	}
	var created auth.LegalHold
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.ID == "" {
		t.Fatal("issued hold must have ID")
	}

	// List
	resp2, _ := http.Get(srv.URL + "/api/v1/tenants/T/legal-hold")
	var holds []auth.LegalHold
	_ = json.NewDecoder(resp2.Body).Decode(&holds)
	resp2.Body.Close()
	if len(holds) != 1 {
		t.Fatalf("list len=%d, want 1", len(holds))
	}

	// Release
	resp3, _ := http.Post(srv.URL+"/api/v1/tenants/T/legal-hold/"+created.ID+"/release", "", nil)
	if resp3.StatusCode != http.StatusNoContent {
		t.Fatalf("release status=%d", resp3.StatusCode)
	}
	resp3.Body.Close()
}

func TestLegalHoldHandler_ReleaseRejectsCrossTenant(t *testing.T) {
	store := auth.NewMemoryLegalHoldStore()
	h := &LegalHoldHandler{Store: store}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Issue under tenant A.
	body, _ := json.Marshal(CreateRequest{Bucket: "b", ObjectKey: "k", Reason: "case-1", IssuedBy: "ops@x"})
	resp, err := http.Post(srv.URL+"/api/v1/tenants/A/legal-hold", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var created auth.LegalHold
	_ = json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	// Try to release the same hold ID under tenant B.
	resp2, _ := http.Post(srv.URL+"/api/v1/tenants/B/legal-hold/"+created.ID+"/release", "", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant release status=%d, want 404", resp2.StatusCode)
	}

	// Hold must still be active.
	got, err := store.Get(httptest.NewRequest("GET", "/", nil).Context(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Released {
		t.Fatal("hold was released by cross-tenant request")
	}
}

func TestLegalHoldHandler_RejectsMissingFields(t *testing.T) {
	h := &LegalHoldHandler{Store: auth.NewMemoryLegalHoldStore()}
	srv := httptest.NewServer(h)
	defer srv.Close()
	body, _ := json.Marshal(CreateRequest{Reason: "", IssuedBy: ""})
	resp, _ := http.Post(srv.URL+"/api/v1/tenants/T/legal-hold", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400", resp.StatusCode)
	}
}
