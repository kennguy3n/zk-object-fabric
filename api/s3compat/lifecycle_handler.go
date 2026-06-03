// WS8.2 — S3 bucket lifecycle configuration (`?lifecycle`).
//
// Implements the bucket-level lifecycle sub-resource
// (Put/Get/DeleteBucketLifecycleConfiguration), persisted through
// metadata/bucket_config. The stored rule set is consumed by the
// daily background lifecycle evaluator (package lifecycle/evaluator),
// which expires objects, aborts stale multipart uploads, and (in a
// later slice) transitions objects between storage tiers. This file
// owns only the XML<->domain mapping and the request handlers; it
// performs no object I/O.
package s3compat

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
)

// ---- XML document types ----

type lifecycleTagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

// lifecycleAndXML is the <And> container S3 requires whenever a filter
// combines more than one predicate (e.g. a prefix and one or more
// tags, or two size bounds).
type lifecycleAndXML struct {
	Prefix                string            `xml:"Prefix,omitempty"`
	Tags                  []lifecycleTagXML `xml:"Tag"`
	ObjectSizeGreaterThan *int64            `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64            `xml:"ObjectSizeLessThan"`
}

// lifecycleFilterXML is the <Filter> element. At most one of its
// single-predicate fields (Prefix, Tag, ObjectSize*) or the multi-
// predicate <And> child is set, matching S3.
type lifecycleFilterXML struct {
	Prefix                *string          `xml:"Prefix"`
	Tag                   *lifecycleTagXML `xml:"Tag"`
	ObjectSizeGreaterThan *int64           `xml:"ObjectSizeGreaterThan"`
	ObjectSizeLessThan    *int64           `xml:"ObjectSizeLessThan"`
	And                   *lifecycleAndXML `xml:"And"`
}

type lifecycleExpirationXML struct {
	Days                      int    `xml:"Days,omitempty"`
	Date                      string `xml:"Date,omitempty"`
	ExpiredObjectDeleteMarker bool   `xml:"ExpiredObjectDeleteMarker,omitempty"`
}

type lifecycleTransitionXML struct {
	Days         int    `xml:"Days,omitempty"`
	Date         string `xml:"Date,omitempty"`
	StorageClass string `xml:"StorageClass,omitempty"`
}

type lifecycleAbortXML struct {
	DaysAfterInitiation int `xml:"DaysAfterInitiation,omitempty"`
}

type lifecycleRuleXML struct {
	ID string `xml:"ID,omitempty"`
	// Prefix is the legacy rule-level prefix from the original
	// (pre-Filter) lifecycle API. AWS still accepts it; it is mutually
	// exclusive with Filter.
	Prefix      *string                  `xml:"Prefix"`
	Status      string                   `xml:"Status"`
	Filter      *lifecycleFilterXML      `xml:"Filter"`
	Expiration  *lifecycleExpirationXML  `xml:"Expiration"`
	Transitions []lifecycleTransitionXML `xml:"Transition"`
	Abort       *lifecycleAbortXML       `xml:"AbortIncompleteMultipartUpload"`
}

// lifecycleConfiguration is the PUT/GET ?lifecycle body
// (<LifecycleConfiguration>).
type lifecycleConfiguration struct {
	XMLName xml.Name           `xml:"LifecycleConfiguration"`
	XMLNS   string             `xml:"xmlns,attr,omitempty"`
	Rules   []lifecycleRuleXML `xml:"Rule"`
}

// lifecycleDateLayouts are the instant formats S3 accepts for a
// <Date>. AWS requires midnight UTC; we parse leniently and let the
// evaluator treat the value as an absolute cutoff.
var lifecycleDateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02",
}

func parseLifecycleDate(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	for _, layout := range lifecycleDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid Date %q: want an ISO-8601 instant", s)
}

// lifecycleConfigFromXML maps the parsed XML document onto the domain
// Config. It returns a MalformedXML-worthy error for structural
// problems the domain Valid() cannot express (bad dates, Filter+Prefix
// both set); semantic validation (actions present, mutual exclusion)
// is left to Config.Valid.
func lifecycleConfigFromXML(doc lifecycleConfiguration) (lifecycle.Config, error) {
	rules := make([]lifecycle.Rule, len(doc.Rules))
	for i, r := range doc.Rules {
		dr := lifecycle.Rule{ID: r.ID, Status: r.Status}

		if r.Filter != nil && r.Prefix != nil {
			return lifecycle.Config{}, fmt.Errorf("rule %d: Filter and top-level Prefix are mutually exclusive", i)
		}
		switch {
		case r.Filter != nil:
			f, err := lifecycleFilterFromXML(*r.Filter)
			if err != nil {
				return lifecycle.Config{}, fmt.Errorf("rule %d: %w", i, err)
			}
			dr.Filter = f
		case r.Prefix != nil:
			dr.Filter = lifecycle.Filter{Prefix: *r.Prefix}
		}

		if e := r.Expiration; e != nil {
			date, err := parseLifecycleDate(e.Date)
			if err != nil {
				return lifecycle.Config{}, fmt.Errorf("rule %d: expiration: %w", i, err)
			}
			dr.Expiration = &lifecycle.Expiration{
				Days:                      e.Days,
				Date:                      date,
				ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker,
			}
		}

		for j, t := range r.Transitions {
			date, err := parseLifecycleDate(t.Date)
			if err != nil {
				return lifecycle.Config{}, fmt.Errorf("rule %d: transition %d: %w", i, j, err)
			}
			dr.Transitions = append(dr.Transitions, lifecycle.Transition{
				Days:         t.Days,
				Date:         date,
				StorageClass: t.StorageClass,
			})
		}

		if a := r.Abort; a != nil {
			dr.AbortIncompleteMultipartUpload = &lifecycle.AbortIncompleteMultipartUpload{
				DaysAfterInitiation: a.DaysAfterInitiation,
			}
		}
		rules[i] = dr
	}
	return lifecycle.Config{Rules: rules}, nil
}

func lifecycleFilterFromXML(f lifecycleFilterXML) (lifecycle.Filter, error) {
	// <And> wins when present; S3 forbids combining it with the
	// sibling single-predicate fields.
	if f.And != nil {
		if f.Prefix != nil || f.Tag != nil || f.ObjectSizeGreaterThan != nil || f.ObjectSizeLessThan != nil {
			return lifecycle.Filter{}, fmt.Errorf("filter: <And> must not be combined with sibling predicates")
		}
		out := lifecycle.Filter{
			Prefix:                f.And.Prefix,
			ObjectSizeGreaterThan: f.And.ObjectSizeGreaterThan,
			ObjectSizeLessThan:    f.And.ObjectSizeLessThan,
		}
		if len(f.And.Tags) > 0 {
			out.Tags = make(map[string]string, len(f.And.Tags))
			for _, t := range f.And.Tags {
				out.Tags[t.Key] = t.Value
			}
		}
		return out, nil
	}
	out := lifecycle.Filter{
		ObjectSizeGreaterThan: f.ObjectSizeGreaterThan,
		ObjectSizeLessThan:    f.ObjectSizeLessThan,
	}
	if f.Prefix != nil {
		out.Prefix = *f.Prefix
	}
	if f.Tag != nil {
		out.Tags = map[string]string{f.Tag.Key: f.Tag.Value}
	}
	return out, nil
}

func lifecycleConfigToXML(cfg lifecycle.Config) lifecycleConfiguration {
	doc := lifecycleConfiguration{XMLNS: s3XMLNamespace, Rules: make([]lifecycleRuleXML, len(cfg.Rules))}
	for i, r := range cfg.Rules {
		xr := lifecycleRuleXML{ID: r.ID, Status: r.Status}
		if !r.Filter.Empty() {
			xr.Filter = lifecycleFilterToXML(r.Filter)
		} else {
			// An empty filter still round-trips as an empty <Filter/>
			// element, matching the AWS GET response shape.
			xr.Filter = &lifecycleFilterXML{}
		}
		if e := r.Expiration; e != nil {
			xe := &lifecycleExpirationXML{
				Days:                      e.Days,
				ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker,
			}
			if !e.Date.IsZero() {
				xe.Date = e.Date.UTC().Format(time.RFC3339)
			}
			xr.Expiration = xe
		}
		for _, t := range r.Transitions {
			xt := lifecycleTransitionXML{Days: t.Days, StorageClass: t.StorageClass}
			if !t.Date.IsZero() {
				xt.Date = t.Date.UTC().Format(time.RFC3339)
			}
			xr.Transitions = append(xr.Transitions, xt)
		}
		if a := r.AbortIncompleteMultipartUpload; a != nil {
			xr.Abort = &lifecycleAbortXML{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		doc.Rules[i] = xr
	}
	return doc
}

func lifecycleFilterToXML(f lifecycle.Filter) *lifecycleFilterXML {
	// Count predicates: more than one requires the <And> wrapper.
	predicates := 0
	if f.Prefix != "" {
		predicates++
	}
	predicates += len(f.Tags)
	if f.ObjectSizeGreaterThan != nil {
		predicates++
	}
	if f.ObjectSizeLessThan != nil {
		predicates++
	}
	if predicates <= 1 {
		out := &lifecycleFilterXML{
			ObjectSizeGreaterThan: f.ObjectSizeGreaterThan,
			ObjectSizeLessThan:    f.ObjectSizeLessThan,
		}
		if f.Prefix != "" {
			p := f.Prefix
			out.Prefix = &p
		}
		for k, v := range f.Tags {
			out.Tag = &lifecycleTagXML{Key: k, Value: v}
		}
		return out
	}
	and := &lifecycleAndXML{
		Prefix:                f.Prefix,
		ObjectSizeGreaterThan: f.ObjectSizeGreaterThan,
		ObjectSizeLessThan:    f.ObjectSizeLessThan,
	}
	for k, v := range f.Tags {
		and.Tags = append(and.Tags, lifecycleTagXML{Key: k, Value: v})
	}
	return &lifecycleFilterXML{And: and}
}

// ---- handlers ----

// PutBucketLifecycleConfiguration handles PUT /{bucket}?lifecycle. It
// replaces the bucket's lifecycle configuration with the supplied rule
// set.
func (h *Handler) PutBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "lifecycle is a bucket-level sub-resource; path must be /{bucket}?lifecycle", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket lifecycle is not configured on this gateway", r.URL.Path)
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	var doc lifecycleConfiguration
	if err := xml.Unmarshal(raw, &doc); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse LifecycleConfiguration: "+err.Error(), r.URL.Path)
		return
	}
	cfg, err := lifecycleConfigFromXML(doc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), r.URL.Path)
		return
	}
	if err := cfg.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.SetLifecycle(r.Context(), tenantID, bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "LifecyclePutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketLifecycleConfiguration handles GET /{bucket}?lifecycle. It
// returns the bucket's lifecycle configuration, or 404
// NoSuchLifecycleConfiguration when the bucket has none, matching AWS.
func (h *Handler) GetBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?lifecycle", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket lifecycle is not configured on this gateway", r.URL.Path)
		return
	}
	cfg, err := h.cfg.BucketConfig.GetLifecycle(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "LifecycleGetFailed", err.Error(), r.URL.Path)
		return
	}
	if cfg.Empty() {
		writeError(w, http.StatusNotFound, "NoSuchLifecycleConfiguration", "The lifecycle configuration does not exist", r.URL.Path)
		return
	}
	writeXMLDoc(w, lifecycleConfigToXML(cfg))
}

// DeleteBucketLifecycleConfiguration handles DELETE
// /{bucket}?lifecycle. It removes the bucket's lifecycle configuration
// and returns 204 No Content. Deleting a bucket with no lifecycle
// configuration is a no-op success, matching AWS's idempotent
// DeleteBucketLifecycle.
func (h *Handler) DeleteBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "lifecycle is a bucket-level sub-resource; path must be /{bucket}?lifecycle", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket lifecycle is not configured on this gateway", r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.DeleteLifecycle(r.Context(), tenantID, bucket); err != nil {
		writeError(w, http.StatusInternalServerError, "LifecycleDeleteFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
