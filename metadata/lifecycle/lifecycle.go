// Package lifecycle defines the provider-neutral domain types for S3
// bucket lifecycle configuration (WS8.2): the per-bucket set of
// lifecycle rules and the matching/age logic the daily evaluator uses
// to decide which objects to expire, which incomplete multipart
// uploads to abort, and (in a future slice) which objects to
// transition between storage tiers.
//
// It carries no persistence of its own — bucket-level Config is stored
// through metadata/bucket_config (the per-bucket S3 config store,
// shared with versioning, Object Lock, and CORS). The api/s3compat
// layer maps between this type and the <LifecycleConfiguration> XML
// document; the lifecycle/evaluator package consumes the matching and
// age helpers to act on objects. This package only owns the value
// semantics (valid rules, filter matching, expiry computation).
package lifecycle

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// maxRules is the S3 limit on the number of lifecycle rules per
// bucket.
const maxRules = 1000

// Rule status values. A Disabled rule is persisted and round-tripped
// through Get/PutBucketLifecycleConfiguration but is skipped by the
// evaluator, matching AWS.
const (
	StatusEnabled  = "Enabled"
	StatusDisabled = "Disabled"
)

// validStorageClasses is the set of S3 storage classes a Transition
// may target. STANDARD and REDUCED_REDUNDANCY are intentionally absent
// — S3 rejects them as transition targets because they are not
// archival tiers.
var validStorageClasses = map[string]bool{
	"STANDARD_IA":         true,
	"ONEZONE_IA":          true,
	"INTELLIGENT_TIERING": true,
	"GLACIER":             true,
	"GLACIER_IR":          true,
	"DEEP_ARCHIVE":        true,
}

// Filter selects which objects a rule applies to. An empty Filter
// (zero value) matches every object in the bucket. When multiple
// predicates are set they are ANDed, mirroring the S3 <And> container
// (the API layer flattens <And> into this struct).
type Filter struct {
	// Prefix restricts the rule to object keys beginning with this
	// string. Empty means no prefix restriction.
	Prefix string
	// Tags restricts the rule to objects carrying every one of these
	// tag key=value pairs. Nil/empty means no tag restriction.
	Tags map[string]string
	// ObjectSizeGreaterThan, when non-nil, restricts the rule to
	// objects strictly larger than this many bytes.
	ObjectSizeGreaterThan *int64
	// ObjectSizeLessThan, when non-nil, restricts the rule to objects
	// strictly smaller than this many bytes.
	ObjectSizeLessThan *int64
}

// Empty reports whether the filter selects every object (no
// predicates set).
func (f Filter) Empty() bool {
	return f.Prefix == "" && len(f.Tags) == 0 &&
		f.ObjectSizeGreaterThan == nil && f.ObjectSizeLessThan == nil
}

// Expiration is the rule's object-expiration action. Exactly one of
// Days, Date, or ExpiredObjectDeleteMarker is set (the API layer and
// Valid enforce this). Days expires an object that many days after
// its creation; Date expires every matching object once that absolute
// instant has passed; ExpiredObjectDeleteMarker cleans up delete
// markers whose only remaining version is the marker itself.
type Expiration struct {
	// Days is the object age, in days since creation, at which the
	// object expires. Zero means "not set" (Date or
	// ExpiredObjectDeleteMarker carries the action instead).
	Days int
	// Date is an absolute expiration instant. Zero (IsZero) means
	// "not set". AWS requires midnight UTC; the API layer preserves
	// whatever the client sent.
	Date time.Time
	// ExpiredObjectDeleteMarker, when true, asks the evaluator to
	// remove a delete marker that has no non-current versions behind
	// it. Mutually exclusive with Days/Date.
	ExpiredObjectDeleteMarker bool
}

// Transition is a single storage-class transition action. Exactly one
// of Days or Date is set, and StorageClass names an archival tier.
//
// NOTE (WS8.2 scope): transition rules are validated, persisted, and
// served at full S3 fidelity, but the daily evaluator does not yet
// execute them — moving object data between tiers reuses the
// migration tiering engine (docs/PROPOSAL.md §4) and lands in a
// follow-up slice. Only Expiration and AbortIncompleteMultipartUpload
// are acted on today.
type Transition struct {
	Days         int
	Date         time.Time
	StorageClass string
}

// AbortIncompleteMultipartUpload asks the evaluator to abort multipart
// uploads that were initiated more than DaysAfterInitiation days ago
// and never completed, reclaiming their staged parts.
type AbortIncompleteMultipartUpload struct {
	DaysAfterInitiation int
}

// Rule is a single S3 lifecycle rule (<Rule>). A rule must carry at
// least one action (Expiration, one or more Transitions, or
// AbortIncompleteMultipartUpload).
type Rule struct {
	// ID is an optional, opaque rule identifier (<ID>). AWS limits it
	// to 255 characters; it has no matching semantics but must be
	// unique within the configuration.
	ID string
	// Status is StatusEnabled or StatusDisabled.
	Status string
	// Filter selects the objects the rule applies to.
	Filter Filter
	// Expiration is the object-expiration action, or nil.
	Expiration *Expiration
	// Transitions are storage-class transition actions (stored/served
	// only — see the Transition doc).
	Transitions []Transition
	// AbortIncompleteMultipartUpload is the stale-upload cleanup
	// action, or nil.
	AbortIncompleteMultipartUpload *AbortIncompleteMultipartUpload
}

// Enabled reports whether the rule's status is Enabled (the evaluator
// skips Disabled rules).
func (r Rule) Enabled() bool {
	return r.Status == StatusEnabled
}

// Config is the bucket-level lifecycle configuration set by
// PutBucketLifecycleConfiguration. An empty Config (no rules) means
// the bucket has no lifecycle configuration, which
// GetBucketLifecycleConfiguration surfaces as 404.
type Config struct {
	Rules []Rule
}

// Empty reports whether the bucket has no lifecycle rules configured.
func (c Config) Empty() bool {
	return len(c.Rules) == 0
}

// Valid checks that the configuration is well-formed per the S3
// lifecycle contract. It is called by PutBucketLifecycleConfiguration
// before persisting.
func (c Config) Valid() error {
	if len(c.Rules) == 0 {
		return errors.New("lifecycle: configuration must contain at least one Rule")
	}
	if len(c.Rules) > maxRules {
		return fmt.Errorf("lifecycle: at most %d Rules are allowed", maxRules)
	}
	seenIDs := make(map[string]bool, len(c.Rules))
	for i, r := range c.Rules {
		if err := r.valid(); err != nil {
			return fmt.Errorf("lifecycle: rule %d: %w", i, err)
		}
		if r.ID != "" {
			if seenIDs[r.ID] {
				return fmt.Errorf("lifecycle: rule %d: duplicate rule ID %q", i, r.ID)
			}
			seenIDs[r.ID] = true
		}
	}
	return nil
}

func (r Rule) valid() error {
	if r.Status != StatusEnabled && r.Status != StatusDisabled {
		return fmt.Errorf("status %q must be Enabled or Disabled", r.Status)
	}
	if len(r.ID) > 255 {
		return errors.New("ID must be at most 255 characters")
	}
	if err := r.Filter.valid(); err != nil {
		return err
	}
	hasAction := r.Expiration != nil || len(r.Transitions) > 0 || r.AbortIncompleteMultipartUpload != nil
	if !hasAction {
		return errors.New("rule must specify at least one action (Expiration, Transition, or AbortIncompleteMultipartUpload)")
	}
	if r.Expiration != nil {
		if err := r.Expiration.valid(); err != nil {
			return err
		}
	}
	for i, t := range r.Transitions {
		if err := t.valid(); err != nil {
			return fmt.Errorf("transition %d: %w", i, err)
		}
	}
	if a := r.AbortIncompleteMultipartUpload; a != nil {
		if a.DaysAfterInitiation <= 0 {
			return errors.New("AbortIncompleteMultipartUpload.DaysAfterInitiation must be a positive number of days")
		}
		// AWS rejects AbortIncompleteMultipartUpload combined with a
		// tag filter: multipart uploads carry no object tags, so the
		// pairing can never match.
		if len(r.Filter.Tags) > 0 {
			return errors.New("AbortIncompleteMultipartUpload cannot be combined with a tag filter")
		}
	}
	return nil
}

func (f Filter) valid() error {
	for k := range f.Tags {
		if k == "" {
			return errors.New("filter tag key must not be empty")
		}
	}
	if f.ObjectSizeGreaterThan != nil && *f.ObjectSizeGreaterThan < 0 {
		return errors.New("ObjectSizeGreaterThan must not be negative")
	}
	// AWS rejects ObjectSizeLessThan below 1: "< 0 bytes" matches no
	// object, so a zero bound is a useless predicate. ObjectSizeGreaterThan
	// of 0 is accepted (it selects every object larger than zero bytes).
	if f.ObjectSizeLessThan != nil && *f.ObjectSizeLessThan < 1 {
		return errors.New("ObjectSizeLessThan must be a positive number of bytes")
	}
	if f.ObjectSizeGreaterThan != nil && f.ObjectSizeLessThan != nil &&
		*f.ObjectSizeGreaterThan >= *f.ObjectSizeLessThan {
		return errors.New("ObjectSizeGreaterThan must be less than ObjectSizeLessThan")
	}
	return nil
}

func (e Expiration) valid() error {
	// Exactly one of Days / Date / ExpiredObjectDeleteMarker.
	set := 0
	if e.Days != 0 {
		set++
	}
	if !e.Date.IsZero() {
		set++
	}
	if e.ExpiredObjectDeleteMarker {
		set++
	}
	if set == 0 {
		return errors.New("Expiration must set one of Days, Date, or ExpiredObjectDeleteMarker")
	}
	if set > 1 {
		return errors.New("Expiration must set exactly one of Days, Date, or ExpiredObjectDeleteMarker")
	}
	if e.Days < 0 {
		return errors.New("Expiration.Days must be a positive number of days")
	}
	return nil
}

func (t Transition) valid() error {
	if t.StorageClass == "" {
		return errors.New("StorageClass is required")
	}
	if !validStorageClasses[t.StorageClass] {
		return fmt.Errorf("StorageClass %q is not a valid transition target", t.StorageClass)
	}
	daysSet := t.Days != 0
	dateSet := !t.Date.IsZero()
	if daysSet == dateSet {
		return errors.New("transition must set exactly one of Days or Date")
	}
	if t.Days < 0 {
		return errors.New("Transition.Days must be a positive number of days")
	}
	return nil
}

// Matches reports whether the rule's filter selects an object with the
// given key, tag set, and size. A Disabled rule still "matches" by
// filter — callers consult Enabled separately so that disabled rules
// can be reported without being acted on.
func (r Rule) Matches(key string, tags map[string]string, size int64) bool {
	f := r.Filter
	if f.Prefix != "" && !strings.HasPrefix(key, f.Prefix) {
		return false
	}
	for k, v := range f.Tags {
		if tags[k] != v {
			return false
		}
	}
	if f.ObjectSizeGreaterThan != nil && size <= *f.ObjectSizeGreaterThan {
		return false
	}
	if f.ObjectSizeLessThan != nil && size >= *f.ObjectSizeLessThan {
		return false
	}
	return true
}

// ExpiresAt returns the instant at which an object created at
// createdAt expires under this rule, and whether an age/date-based
// expiration applies at all. It is false when the rule has no
// Expiration, when the expiration is an ExpiredObjectDeleteMarker
// action (which is not driven by object age), or when createdAt is the
// zero time for a Days-based rule (an object of unknown age is never
// expired, so the evaluator fails safe).
//
// For a Days-based rule AWS expires the object at midnight UTC on the
// day after (createdAt + Days). We compute createdAt + Days*24h and
// round up to the next UTC midnight to match that boundary behaviour.
func (r Rule) ExpiresAt(createdAt time.Time) (time.Time, bool) {
	e := r.Expiration
	if e == nil || e.ExpiredObjectDeleteMarker {
		return time.Time{}, false
	}
	if !e.Date.IsZero() {
		return e.Date, true
	}
	if e.Days <= 0 || createdAt.IsZero() {
		return time.Time{}, false
	}
	expiry := createdAt.UTC().AddDate(0, 0, e.Days)
	return ceilToUTCMidnight(expiry), true
}

// AbortStaleBefore returns the cutoff instant for the rule's
// AbortIncompleteMultipartUpload action evaluated at now: an upload
// initiated at or before the returned instant is stale and should be
// aborted. The bool is false when the rule has no such action.
func (r Rule) AbortStaleBefore(now time.Time) (time.Time, bool) {
	a := r.AbortIncompleteMultipartUpload
	if a == nil || a.DaysAfterInitiation <= 0 {
		return time.Time{}, false
	}
	return now.UTC().AddDate(0, 0, -a.DaysAfterInitiation), true
}

// ceilToUTCMidnight rounds t up to the next 00:00:00 UTC boundary,
// returning t unchanged when it already sits exactly on midnight.
func ceilToUTCMidnight(t time.Time) time.Time {
	t = t.UTC()
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if t.Equal(midnight) {
		return midnight
	}
	return midnight.AddDate(0, 0, 1)
}
