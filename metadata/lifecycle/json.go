package lifecycle

import (
	"encoding/json"
	"time"
)

// jsonFilter, jsonExpiration, jsonTransition, jsonAbort, jsonRule and
// jsonConfig are the on-disk JSON shapes of the corresponding domain
// types. They are intentionally separate from the in-memory structs so
// the persisted format is decoupled: renaming a domain field never
// silently changes what is already stored. The lowercase tags keep
// stored documents compact and stable across versions.
type jsonFilter struct {
	Prefix                string            `json:"prefix,omitempty"`
	Tags                  map[string]string `json:"tags,omitempty"`
	ObjectSizeGreaterThan *int64            `json:"object_size_gt,omitempty"`
	ObjectSizeLessThan    *int64            `json:"object_size_lt,omitempty"`
}

type jsonExpiration struct {
	Days                      int        `json:"days,omitempty"`
	Date                      *time.Time `json:"date,omitempty"`
	ExpiredObjectDeleteMarker bool       `json:"expired_object_delete_marker,omitempty"`
}

type jsonTransition struct {
	Days         int        `json:"days,omitempty"`
	Date         *time.Time `json:"date,omitempty"`
	StorageClass string     `json:"storage_class,omitempty"`
}

type jsonAbort struct {
	DaysAfterInitiation int `json:"days_after_initiation,omitempty"`
}

type jsonRule struct {
	ID          string           `json:"id,omitempty"`
	Status      string           `json:"status,omitempty"`
	Filter      jsonFilter       `json:"filter,omitempty"`
	Expiration  *jsonExpiration  `json:"expiration,omitempty"`
	Transitions []jsonTransition `json:"transitions,omitempty"`
	Abort       *jsonAbort       `json:"abort_incomplete_multipart_upload,omitempty"`
}

type jsonConfig struct {
	Rules []jsonRule `json:"rules,omitempty"`
}

// MarshalJSON serialises the configuration to the stable persistence
// format used by the Postgres and SQLite stores.
func (c Config) MarshalJSON() ([]byte, error) {
	out := jsonConfig{Rules: make([]jsonRule, len(c.Rules))}
	for i, r := range c.Rules {
		jr := jsonRule{
			ID:     r.ID,
			Status: r.Status,
			Filter: jsonFilter{
				Prefix:                r.Filter.Prefix,
				Tags:                  r.Filter.Tags,
				ObjectSizeGreaterThan: r.Filter.ObjectSizeGreaterThan,
				ObjectSizeLessThan:    r.Filter.ObjectSizeLessThan,
			},
		}
		if e := r.Expiration; e != nil {
			je := jsonExpiration{
				Days:                      e.Days,
				ExpiredObjectDeleteMarker: e.ExpiredObjectDeleteMarker,
			}
			if !e.Date.IsZero() {
				d := e.Date
				je.Date = &d
			}
			jr.Expiration = &je
		}
		if len(r.Transitions) > 0 {
			jr.Transitions = make([]jsonTransition, len(r.Transitions))
			for j, t := range r.Transitions {
				jt := jsonTransition{Days: t.Days, StorageClass: t.StorageClass}
				if !t.Date.IsZero() {
					d := t.Date
					jt.Date = &d
				}
				jr.Transitions[j] = jt
			}
		}
		if a := r.AbortIncompleteMultipartUpload; a != nil {
			jr.Abort = &jsonAbort{DaysAfterInitiation: a.DaysAfterInitiation}
		}
		out.Rules[i] = jr
	}
	return json.Marshal(out)
}

// UnmarshalJSON parses the stable persistence format produced by
// MarshalJSON.
func (c *Config) UnmarshalJSON(b []byte) error {
	var in jsonConfig
	if err := json.Unmarshal(b, &in); err != nil {
		return err
	}
	rules := make([]Rule, len(in.Rules))
	for i, jr := range in.Rules {
		r := Rule{
			ID:     jr.ID,
			Status: jr.Status,
			Filter: Filter{
				Prefix:                jr.Filter.Prefix,
				Tags:                  jr.Filter.Tags,
				ObjectSizeGreaterThan: jr.Filter.ObjectSizeGreaterThan,
				ObjectSizeLessThan:    jr.Filter.ObjectSizeLessThan,
			},
		}
		if je := jr.Expiration; je != nil {
			e := Expiration{
				Days:                      je.Days,
				ExpiredObjectDeleteMarker: je.ExpiredObjectDeleteMarker,
			}
			if je.Date != nil {
				e.Date = *je.Date
			}
			r.Expiration = &e
		}
		if len(jr.Transitions) > 0 {
			r.Transitions = make([]Transition, len(jr.Transitions))
			for j, jt := range jr.Transitions {
				t := Transition{Days: jt.Days, StorageClass: jt.StorageClass}
				if jt.Date != nil {
					t.Date = *jt.Date
				}
				r.Transitions[j] = t
			}
		}
		if ja := jr.Abort; ja != nil {
			r.AbortIncompleteMultipartUpload = &AbortIncompleteMultipartUpload{
				DaysAfterInitiation: ja.DaysAfterInitiation,
			}
		}
		rules[i] = r
	}
	c.Rules = rules
	return nil
}
