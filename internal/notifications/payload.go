package notifications

import (
	"time"

	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

// Event is a single object-level event the gateway emits for delivery
// to the bucket's configured notification destinations. It is the
// internal representation; the dispatcher looks up the bucket's
// notification.Config, matches this event against its rules, and POSTs
// the rendered payload to each matching webhook Endpoint.
type Event struct {
	// TenantID and Bucket scope the lookup of the notification
	// configuration; they are never part of the delivered payload's
	// public S3 shape beyond the bucket name.
	TenantID string
	Bucket   string

	// Name is the specific leaf event (e.g. ObjectCreatedPut). A
	// configured rule subscribed to the wildcard class still matches
	// — the matching is done by notification.Config.Match.
	Name notification.EventType

	// ObjectKey is the key the event concerns.
	ObjectKey string

	// SizeBytes is the object size for create events; zero for
	// removes.
	SizeBytes int64

	// ETag is the object ETag for create events; empty for removes.
	ETag string

	// VersionID is the affected version when the bucket is versioned;
	// empty otherwise.
	VersionID string

	// Time is when the operation completed (UTC).
	Time time.Time

	// RequestID correlates the event with the originating S3 request
	// and the audit trail.
	RequestID string

	// SourceIP is the client address of the originating request, when
	// known.
	SourceIP string

	// Region is the gateway's configured region/zone, surfaced as the
	// awsRegion field of the record.
	Region string

	// Sequencer is a monotonically increasing hex token a consumer can
	// use to order events for the same key. The dispatcher fills it in
	// when empty.
	Sequencer string
}

// s3EventEnvelope is the top-level S3 event-notification JSON document
// ({"Records":[...]}). The gateway delivers exactly one record per
// POST, matching how AWS fans out per-destination.
type s3EventEnvelope struct {
	Records []s3EventRecord `json:"Records"`
}

type s3EventRecord struct {
	EventVersion string         `json:"eventVersion"`
	EventSource  string         `json:"eventSource"`
	AwsRegion    string         `json:"awsRegion"`
	EventTime    string         `json:"eventTime"`
	EventName    string         `json:"eventName"`
	UserIdentity s3UserIdentity `json:"userIdentity"`
	RequestParms s3RequestParms `json:"requestParameters"`
	ResponseElem s3ResponseElem `json:"responseElements"`
	S3           s3Entity       `json:"s3"`
}

type s3UserIdentity struct {
	PrincipalID string `json:"principalId"`
}

type s3RequestParms struct {
	SourceIPAddress string `json:"sourceIPAddress"`
}

type s3ResponseElem struct {
	RequestID string `json:"x-amz-request-id"`
}

type s3Entity struct {
	S3SchemaVersion string         `json:"s3SchemaVersion"`
	ConfigurationID string         `json:"configurationId"`
	Bucket          s3BucketEntity `json:"bucket"`
	Object          s3ObjectEntity `json:"object"`
}

type s3BucketEntity struct {
	Name string `json:"name"`
}

type s3ObjectEntity struct {
	Key string `json:"key"`
	// Size is a pointer so the field tracks AWS exactly: every
	// ObjectCreated record carries size (a *int64 to 0 still
	// serialises as "size":0 for a 0-byte object), while ObjectRemoved
	// records omit it entirely (nil → dropped by omitempty). A plain
	// int64 could not distinguish "0-byte create" from "remove".
	Size      *int64 `json:"size,omitempty"`
	ETag      string `json:"eTag,omitempty"`
	VersionID string `json:"versionId,omitempty"`
	Sequencer string `json:"sequencer"`
}

// render builds the single-record S3 event document for one matched
// rule. configurationID is the matched rule's ID (may be empty).
func (e Event) render(configurationID string) s3EventEnvelope {
	// AWS includes object size only on ObjectCreated records (even for
	// a 0-byte object); ObjectRemoved records omit it. Bind a pointer
	// for creates so a true 0 is emitted, and leave it nil otherwise.
	var size *int64
	if e.Name.IsObjectCreated() {
		s := e.SizeBytes
		size = &s
	}
	return s3EventEnvelope{
		Records: []s3EventRecord{{
			EventVersion: "2.1",
			EventSource:  "zkof:s3",
			AwsRegion:    e.Region,
			EventTime:    e.Time.UTC().Format(time.RFC3339Nano),
			EventName:    string(e.Name),
			UserIdentity: s3UserIdentity{PrincipalID: e.TenantID},
			RequestParms: s3RequestParms{SourceIPAddress: e.SourceIP},
			ResponseElem: s3ResponseElem{RequestID: e.RequestID},
			S3: s3Entity{
				S3SchemaVersion: "1.0",
				ConfigurationID: configurationID,
				Bucket:          s3BucketEntity{Name: e.Bucket},
				Object: s3ObjectEntity{
					Key:       e.ObjectKey,
					Size:      size,
					ETag:      e.ETag,
					VersionID: e.VersionID,
					Sequencer: e.Sequencer,
				},
			},
		}},
	}
}
