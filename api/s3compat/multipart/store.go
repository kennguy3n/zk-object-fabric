// Package multipart holds the S3 multipart-upload session store used
// by the gateway's S3-compatible handler.
//
// A multipart upload is a three-phase operation: the client calls
// CreateMultipartUpload, streams one or more UploadPart requests, and
// finally calls CompleteMultipartUpload (or AbortMultipartUpload). The
// gateway needs a small piece of server-side state between those calls
// to track which parts have landed and where they are stored.
//
// The in-memory Store implemented here is sufficient for a single
// gateway node. Multi-node fleets should back the same interface with
// a durable control-plane store (Postgres, Redis, etc.) so that a
// reconnecting client can complete an upload against a different node.
package multipart

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata"
)

// Errors returned by Store implementations.
var (
	// ErrNotFound is returned when the requested upload ID is not
	// recognised by the store.
	ErrNotFound = errors.New("multipart: upload not found")

	// ErrPartNotFound is returned when CompleteMultipartUpload
	// references a part that was never uploaded.
	ErrPartNotFound = errors.New("multipart: part not found")

	// ErrPartETagMismatch is returned when the client-supplied ETag
	// does not match the one recorded at UploadPart time.
	ErrPartETagMismatch = errors.New("multipart: part etag mismatch")

	// ErrTenantMismatch is returned when Abort / Complete is called
	// with a tenant that does not own the upload. The upload is
	// left intact so the legitimate tenant can still finish it.
	ErrTenantMismatch = errors.New("multipart: tenant mismatch")

	// ErrUploadMismatch is returned when Complete is called with a
	// (bucket, object key) pair that does not match the upload's
	// recorded target. The upload is left intact.
	ErrUploadMismatch = errors.New("multipart: bucket or object key mismatch")
)

// Part describes a single uploaded part.
type Part struct {
	PartNumber int
	PieceID    string
	Backend    string
	ETag       string
	SizeBytes  int64
	UploadedAt time.Time
}

// Upload is the server-side record of an in-flight multipart session.
type Upload struct {
	ID        string
	TenantID  string
	Bucket    string
	ObjectKey string
	Backend   string
	Policy    metadata.PlacementPolicy
	CreatedAt time.Time

	mu    sync.Mutex
	parts map[int]Part
}

// Parts returns a copy of the part set keyed by PartNumber. Callers
// may mutate the returned map safely.
func (u *Upload) Parts() map[int]Part {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make(map[int]Part, len(u.parts))
	for k, v := range u.parts {
		out[k] = v
	}
	return out
}

// SortedParts returns the recorded parts in ascending PartNumber
// order. This is the order CompleteMultipartUpload uses when
// assembling the final manifest.
func (u *Upload) SortedParts() []Part {
	parts := u.Parts()
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
	return out
}

// Store is the abstract interface over multipart upload state.
//
// Upload is passed and returned by pointer because it embeds a
// sync.Mutex that must not be copied.
//
// Complete and Abort take the caller's tenant / bucket / object-key
// claim so the authorization check is atomic with the state-changing
// delete: a tenant mismatch returns ErrTenantMismatch without
// removing the upload.
type Store interface {
	Create(upload *Upload) error
	Get(uploadID string) (*Upload, error)
	PutPart(uploadID string, part Part) error
	Complete(uploadID, tenantID, bucket, objectKey string, expected []PartReference) ([]Part, *Upload, error)
	Abort(uploadID, tenantID string) (*Upload, []Part, error)
	// List returns all in-flight uploads for (tenantID, bucket).
	// Primarily used by the ListMultipartUploads administrative
	// endpoint; dev / tests also consult it.
	List(tenantID, bucket string) []*Upload
}

// PartReference is the client's claim in CompleteMultipartUpload that
// a part with the given number and ETag is expected to be present.
type PartReference struct {
	PartNumber int
	ETag       string
}

// MemoryStore is an in-memory Store. It is safe for concurrent use by
// the HTTP handler. Operator-facing deployments should swap this for a
// shared backing store so uploads survive node restarts.
type MemoryStore struct {
	mu      sync.Mutex
	uploads map[string]*Upload
}

// NewMemoryStore returns an empty MemoryStore ready for use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{uploads: map[string]*Upload{}}
}

// Create records a new in-flight upload. The caller should pass a
// freshly-allocated *Upload; the store takes ownership of the
// pointer and initialises its parts map.
func (s *MemoryStore) Create(upload *Upload) error {
	if upload == nil || upload.ID == "" {
		return errors.New("multipart: upload id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.uploads[upload.ID]; exists {
		return errors.New("multipart: upload id already exists")
	}
	upload.parts = map[int]Part{}
	s.uploads[upload.ID] = upload
	return nil
}

// Get returns the upload keyed by ID or ErrNotFound.
func (s *MemoryStore) Get(uploadID string) (*Upload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[uploadID]
	if !ok {
		return nil, ErrNotFound
	}
	return u, nil
}

// PutPart records (or overwrites) a part for an in-flight upload. S3
// semantics allow re-uploading a part; the latest write wins.
func (s *MemoryStore) PutPart(uploadID string, part Part) error {
	u, err := s.Get(uploadID)
	if err != nil {
		return err
	}
	if part.PartNumber <= 0 {
		return errors.New("multipart: part_number must be positive")
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	u.parts[part.PartNumber] = part
	return nil
}

// Complete finalises the upload. It validates that the caller's
// tenant / bucket / object-key match the upload's recorded target
// (authorization check is atomic with the delete) and that every
// PartReference names a part that was uploaded with the claimed
// ETag. On success the upload is removed from the store and the
// sorted part list is returned for manifest assembly.
func (s *MemoryStore) Complete(uploadID, tenantID, bucket, objectKey string, expected []PartReference) ([]Part, *Upload, error) {
	s.mu.Lock()
	u, ok := s.uploads[uploadID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	if u.TenantID != tenantID {
		s.mu.Unlock()
		return nil, nil, ErrTenantMismatch
	}
	if u.Bucket != bucket || u.ObjectKey != objectKey {
		s.mu.Unlock()
		return nil, nil, ErrUploadMismatch
	}
	delete(s.uploads, uploadID)
	s.mu.Unlock()

	u.mu.Lock()
	defer u.mu.Unlock()

	result := make([]Part, 0, len(expected))
	for _, ref := range expected {
		part, ok := u.parts[ref.PartNumber]
		if !ok {
			// Re-insert so AbortMultipartUpload can still clean
			// up if the client retries. Actual cleanup of
			// already-uploaded parts is the caller's job.
			s.mu.Lock()
			s.uploads[uploadID] = u
			s.mu.Unlock()
			return nil, nil, ErrPartNotFound
		}
		if ref.ETag != "" && part.ETag != "" && stripQuotes(ref.ETag) != stripQuotes(part.ETag) {
			s.mu.Lock()
			s.uploads[uploadID] = u
			s.mu.Unlock()
			return nil, nil, ErrPartETagMismatch
		}
		result = append(result, part)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PartNumber < result[j].PartNumber })
	return result, u, nil
}

// Abort removes an upload and returns the parts that were uploaded so
// the caller can delete their backing pieces. The tenantID must
// match the upload's recorded tenant; mismatch returns
// ErrTenantMismatch without removing the upload.
func (s *MemoryStore) Abort(uploadID, tenantID string) (*Upload, []Part, error) {
	s.mu.Lock()
	u, ok := s.uploads[uploadID]
	if !ok {
		s.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	if u.TenantID != tenantID {
		s.mu.Unlock()
		return nil, nil, ErrTenantMismatch
	}
	delete(s.uploads, uploadID)
	s.mu.Unlock()

	u.mu.Lock()
	parts := make([]Part, 0, len(u.parts))
	for _, p := range u.parts {
		parts = append(parts, p)
	}
	u.mu.Unlock()
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return u, parts, nil
}

// List returns all uploads scoped to (tenantID, bucket). The returned
// pointers alias the store's internal state; callers must not mutate
// them concurrently with other store operations.
func (s *MemoryStore) List(tenantID, bucket string) []*Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Upload, 0, len(s.uploads))
	for _, u := range s.uploads {
		if u.TenantID != tenantID {
			continue
		}
		if bucket != "" && u.Bucket != bucket {
			continue
		}
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
