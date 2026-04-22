// Package s3compat is the S3-compatible HTTP handler surface for the
// Linode-hosted ZK Gateway. See docs/PROPOSAL.md §3.1.
//
// Phase 1 ships route stubs only. Real request parsing, authentication,
// manifest creation, and ciphertext streaming land in Phase 2.
package s3compat

import (
	"fmt"
	"net/http"
)

// Handler routes S3-compatible requests to the gateway's internal
// pipeline. It intentionally does not satisfy http.Handler itself;
// callers mount specific methods into their router of choice.
type Handler struct {
	// TODO(phase-2): wire in ManifestStore, StorageProvider registry,
	// placement engine, hot cache, and billing counters.
}

// New returns a Handler ready to be wired into an HTTP mux.
func New() *Handler {
	return &Handler{}
}

// Register attaches the S3-compatible routes to mux. Route parsing
// follows S3 path-style addressing (/{bucket}/{key...}).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/", h.dispatch)
}

func (h *Handler) dispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		h.Put(w, r)
	case http.MethodGet:
		if r.URL.Query().Has("list-type") || r.URL.Path == "/" {
			h.List(w, r)
			return
		}
		h.Get(w, r)
	case http.MethodHead:
		h.Head(w, r)
	case http.MethodDelete:
		h.Delete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// Put handles S3 PUT object. It is currently a stub that returns 501.
func (h *Handler) Put(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "PUT")
}

// Get handles S3 GET object.
func (h *Handler) Get(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "GET")
}

// Head handles S3 HEAD object.
func (h *Handler) Head(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "HEAD")
}

// Delete handles S3 DELETE object.
func (h *Handler) Delete(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "DELETE")
}

// List handles S3 LIST bucket (ListObjectsV2).
func (h *Handler) List(w http.ResponseWriter, _ *http.Request) {
	notImplemented(w, "LIST")
}

func notImplemented(w http.ResponseWriter, op string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	fmt.Fprintf(w, "s3compat: %s not implemented in Phase 1 scaffold\n", op)
}
