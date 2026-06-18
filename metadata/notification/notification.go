// Package notification defines the provider-neutral domain types for
// S3 bucket event-notification configuration: the per-bucket
// set of notification rules and the matching logic the gateway uses to
// decide which configured webhook destinations should receive an event
// for a given object operation.
//
// It carries no persistence of its own — bucket-level Config is stored
// through metadata/bucket_config (the per-bucket S3 config store,
// shared with versioning, Object Lock, CORS, and lifecycle). The
// api/s3compat layer maps between this type and the
// <NotificationConfiguration> XML document; the internal/notifications
// dispatcher consumes Config.Match to fan an emitted event out to the
// matching destinations. This package only owns the value semantics
// (valid rules, event-class and key-prefix/suffix matching).
package notification

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// maxRules is the limit on the number of notification rules per
// bucket. AWS's own limit is 100 configurations per bucket; we mirror
// it so a misconfigured client fails the same way.
const maxRules = 100

// EventType is an S3 event name. The gateway emits the specific
// leaf types (e.g. ObjectCreatedPut); a rule may subscribe either to
// a specific leaf or to one of the two wildcard classes
// (ObjectCreatedAll / ObjectRemovedAll), which match every leaf in
// their class.
type EventType string

const (
	// ObjectCreatedAll is the wildcard covering every object-create
	// leaf event. A rule configured with it fires on Put, Copy, and
	// CompleteMultipartUpload.
	ObjectCreatedAll EventType = "s3:ObjectCreated:*"
	// ObjectCreatedPut is emitted by a single-shot PutObject.
	ObjectCreatedPut EventType = "s3:ObjectCreated:Put"
	// ObjectCreatedCopy is emitted by CopyObject.
	ObjectCreatedCopy EventType = "s3:ObjectCreated:Copy"
	// ObjectCreatedCompleteMultipartUpload is emitted when a
	// multipart upload is completed.
	ObjectCreatedCompleteMultipartUpload EventType = "s3:ObjectCreated:CompleteMultipartUpload"

	// ObjectRemovedAll is the wildcard covering every object-remove
	// leaf event. A rule configured with it fires on a hard delete
	// and on delete-marker creation.
	ObjectRemovedAll EventType = "s3:ObjectRemoved:*"
	// ObjectRemovedDelete is emitted when an object (version) is
	// permanently removed.
	ObjectRemovedDelete EventType = "s3:ObjectRemoved:Delete"
	// ObjectRemovedDeleteMarkerCreated is emitted when a versioned
	// bucket records a delete marker rather than removing data.
	ObjectRemovedDeleteMarkerCreated EventType = "s3:ObjectRemoved:DeleteMarkerCreated"
)

// configurableEvents is the set of event names a client may put in a
// rule's <Event>. It includes both wildcard classes and every leaf.
var configurableEvents = map[EventType]bool{
	ObjectCreatedAll:                     true,
	ObjectCreatedPut:                     true,
	ObjectCreatedCopy:                    true,
	ObjectCreatedCompleteMultipartUpload: true,
	ObjectRemovedAll:                     true,
	ObjectRemovedDelete:                  true,
	ObjectRemovedDeleteMarkerCreated:     true,
}

// class returns the wildcard class an event name belongs to, or the
// empty string for an unknown name. Both a leaf (s3:ObjectCreated:Put)
// and its wildcard (s3:ObjectCreated:*) share the class
// "s3:ObjectCreated".
func (e EventType) class() string {
	s := string(e)
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return ""
	}
	return s[:i]
}

// IsObjectCreated reports whether the event is one of the
// s3:ObjectCreated:* leaves (Put/Copy/CompleteMultipartUpload) or the
// wildcard class. The delivered payload uses this to decide whether the
// s3.object entity carries a size field: AWS includes size for every
// ObjectCreated record (including a 0-byte object) and omits it
// entirely for ObjectRemoved records.
func (e EventType) IsObjectCreated() bool {
	return e.class() == "s3:ObjectCreated"
}

// covers reports whether a rule subscribed to event `sub` should fire
// for an emitted leaf event `emitted`. A wildcard subscription matches
// any leaf in its class; otherwise the names must be equal.
func (sub EventType) covers(emitted EventType) bool {
	if sub == emitted {
		return true
	}
	switch sub {
	case ObjectCreatedAll, ObjectRemovedAll:
		return sub.class() == emitted.class()
	default:
		return false
	}
}

// Rule is a single bucket notification rule. A configured rule routes
// matching object events to one webhook Endpoint. AWS expresses the
// destination as a queue/topic/function ARN; this gateway's initial
// transport is a direct webhook (HTTP POST), so Endpoint is the target
// URL.
type Rule struct {
	// ID is an optional, opaque rule identifier. AWS limits it to 255
	// characters and requires uniqueness within the configuration; it
	// has no matching semantics.
	ID string
	// Events are the event names this rule subscribes to. Each must be
	// one of the configurable event names (a leaf or a wildcard
	// class); at least one is required.
	Events []EventType
	// Endpoint is the webhook URL events are POSTed to. It must be an
	// absolute http or https URL.
	Endpoint string
	// Prefix, when non-empty, restricts the rule to object keys that
	// begin with it (the S3 "prefix" FilterRule).
	Prefix string
	// Suffix, when non-empty, restricts the rule to object keys that
	// end with it (the S3 "suffix" FilterRule).
	Suffix string
}

// Config is the bucket-level notification configuration set by
// PutBucketNotificationConfiguration. An empty Config (no rules) means
// the bucket has no notifications configured, which AWS represents as
// an empty <NotificationConfiguration/> document (and PutBucket... with
// an empty body clears any existing configuration).
type Config struct {
	Rules []Rule
}

// Empty reports whether the bucket has no notification rules.
func (c Config) Empty() bool {
	return len(c.Rules) == 0
}

// Valid checks that the configuration is well-formed. It is called by
// PutBucketNotificationConfiguration before persisting. An empty
// configuration is valid: it is how a client clears notifications.
func (c Config) Valid() error {
	if len(c.Rules) > maxRules {
		return fmt.Errorf("notification: at most %d rules are allowed", maxRules)
	}
	seenID := make(map[string]bool, len(c.Rules))
	for i, r := range c.Rules {
		if err := r.valid(); err != nil {
			return fmt.Errorf("notification: rule %d: %w", i, err)
		}
		if r.ID != "" {
			if seenID[r.ID] {
				return fmt.Errorf("notification: duplicate rule ID %q", r.ID)
			}
			seenID[r.ID] = true
		}
	}
	return nil
}

func (r Rule) valid() error {
	if len(r.ID) > 255 {
		return errors.New("ID must be at most 255 characters")
	}
	if len(r.Events) == 0 {
		return errors.New("at least one Event is required")
	}
	for _, e := range r.Events {
		if !configurableEvents[e] {
			return fmt.Errorf("event %q is not a supported event type", e)
		}
	}
	if r.Endpoint == "" {
		return errors.New("a destination Endpoint is required")
	}
	u, err := url.Parse(r.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint %q is not a valid URL: %w", r.Endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("endpoint %q must be an http or https URL", r.Endpoint)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q must include a host", r.Endpoint)
	}
	return nil
}

// Match returns the rules that should fire for an emitted leaf event
// on object key. A rule matches when one of its subscribed events
// covers the emitted event AND the key satisfies the rule's
// prefix/suffix filters. The returned slice preserves configuration
// order and is freshly allocated, so callers may retain it.
func (c Config) Match(emitted EventType, key string) []Rule {
	var out []Rule
	for _, r := range c.Rules {
		if r.Prefix != "" && !strings.HasPrefix(key, r.Prefix) {
			continue
		}
		if r.Suffix != "" && !strings.HasSuffix(key, r.Suffix) {
			continue
		}
		for _, sub := range r.Events {
			if sub.covers(emitted) {
				out = append(out, r)
				break
			}
		}
	}
	return out
}
