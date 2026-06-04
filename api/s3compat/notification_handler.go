// WS8.6 — S3 bucket event notifications (`?notification`).
//
// Implements the bucket-level notification configuration sub-resource
// (Put/GetBucketNotificationConfiguration), persisted through
// metadata/bucket_config. A configured bucket fans object-level events
// (ObjectCreated:* / ObjectRemoved:*) out to tenant-configured webhook
// endpoints via the async dispatcher in internal/notifications, wired
// through the handler's NotificationEmitter (see handler.go's notify).
//
// The on-the-wire document keeps AWS's <Event> and
// <Filter><S3Key><FilterRule> conventions but uses a ZKOF-specific
// <WebhookConfiguration> container with an <Endpoint> element, since
// the initial transport is a plain HTTP webhook (no SNS/SQS ARNs). One
// <WebhookConfiguration> maps 1:1 to a notification.Rule:
//
//	<NotificationConfiguration>
//	  <WebhookConfiguration>
//	    <Id>on-upload</Id>
//	    <Endpoint>https://hooks.example.com/s3</Endpoint>
//	    <Event>s3:ObjectCreated:*</Event>
//	    <Event>s3:ObjectRemoved:Delete</Event>
//	    <Filter><S3Key>
//	      <FilterRule><Name>prefix</Name><Value>logs/</Value></FilterRule>
//	      <FilterRule><Name>suffix</Name><Value>.json</Value></FilterRule>
//	    </S3Key></Filter>
//	  </WebhookConfiguration>
//	</NotificationConfiguration>
//
// An empty <NotificationConfiguration/> clears the bucket's
// configuration, matching AWS's PutBucketNotificationConfiguration with
// an empty body. GetBucketNotificationConfiguration on an unconfigured
// bucket returns an empty document (200), not a 404 — there is no
// NoSuch* error for notifications, unlike CORS.
package s3compat

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
)

// ---- XML document types ----

// filterRuleXML is one <FilterRule> inside <S3Key>. AWS uses Name in
// {prefix, suffix}; any other Name is rejected as malformed.
type filterRuleXML struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

type s3KeyFilterXML struct {
	Rules []filterRuleXML `xml:"FilterRule"`
}

type notificationFilterXML struct {
	S3Key s3KeyFilterXML `xml:"S3Key"`
}

// webhookConfigurationXML is a single notification rule with a webhook
// destination. Events decode into a slice (repeated <Event> elements).
type webhookConfigurationXML struct {
	ID       string                 `xml:"Id,omitempty"`
	Endpoint string                 `xml:"Endpoint"`
	Events   []string               `xml:"Event"`
	Filter   *notificationFilterXML `xml:"Filter,omitempty"`
}

// notificationConfiguration is the PUT/GET ?notification body.
type notificationConfiguration struct {
	XMLName  xml.Name                  `xml:"NotificationConfiguration"`
	XMLNS    string                    `xml:"xmlns,attr,omitempty"`
	Webhooks []webhookConfigurationXML `xml:"WebhookConfiguration"`
}

// notificationConfigFromXML maps the parsed document to the domain
// Config. It returns an error for malformed S3Key filter rules (an
// unknown FilterRule Name or a duplicate prefix/suffix) so the caller
// can surface 400 MalformedXML rather than silently dropping a filter.
func notificationConfigFromXML(doc notificationConfiguration) (notification.Config, error) {
	rules := make([]notification.Rule, len(doc.Webhooks))
	for i, w := range doc.Webhooks {
		events := make([]notification.EventType, len(w.Events))
		for j, e := range w.Events {
			events[j] = notification.EventType(e)
		}
		prefix, suffix, err := filterPrefixSuffix(w.Filter)
		if err != nil {
			return notification.Config{}, err
		}
		rules[i] = notification.Rule{
			ID:       w.ID,
			Events:   events,
			Endpoint: w.Endpoint,
			Prefix:   prefix,
			Suffix:   suffix,
		}
	}
	return notification.Config{Rules: rules}, nil
}

// filterPrefixSuffix extracts the prefix/suffix from an S3Key filter,
// rejecting unknown rule names and duplicates.
func filterPrefixSuffix(f *notificationFilterXML) (prefix, suffix string, err error) {
	if f == nil {
		return "", "", nil
	}
	var sawPrefix, sawSuffix bool
	for _, fr := range f.S3Key.Rules {
		switch fr.Name {
		case "prefix":
			if sawPrefix {
				return "", "", &xmlError{"duplicate prefix FilterRule"}
			}
			sawPrefix, prefix = true, fr.Value
		case "suffix":
			if sawSuffix {
				return "", "", &xmlError{"duplicate suffix FilterRule"}
			}
			sawSuffix, suffix = true, fr.Value
		default:
			return "", "", &xmlError{"FilterRule Name must be prefix or suffix, got " + fr.Name}
		}
	}
	return prefix, suffix, nil
}

type xmlError struct{ msg string }

func (e *xmlError) Error() string { return e.msg }

func notificationConfigToXML(cfg notification.Config) notificationConfiguration {
	doc := notificationConfiguration{XMLNS: s3XMLNamespace, Webhooks: make([]webhookConfigurationXML, len(cfg.Rules))}
	for i, r := range cfg.Rules {
		events := make([]string, len(r.Events))
		for j, e := range r.Events {
			events[j] = string(e)
		}
		w := webhookConfigurationXML{
			ID:       r.ID,
			Endpoint: r.Endpoint,
			Events:   events,
		}
		if r.Prefix != "" || r.Suffix != "" {
			f := &notificationFilterXML{}
			if r.Prefix != "" {
				f.S3Key.Rules = append(f.S3Key.Rules, filterRuleXML{Name: "prefix", Value: r.Prefix})
			}
			if r.Suffix != "" {
				f.S3Key.Rules = append(f.S3Key.Rules, filterRuleXML{Name: "suffix", Value: r.Suffix})
			}
			w.Filter = f
		}
		doc.Webhooks[i] = w
	}
	return doc
}

// ---- bucket-level configuration handlers ----

// PutBucketNotificationConfiguration handles PUT /{bucket}?notification.
// It replaces the bucket's notification configuration with the supplied
// rule set. An empty body clears the configuration.
func (h *Handler) PutBucketNotificationConfiguration(w http.ResponseWriter, r *http.Request) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	bucket, key := parseBucketKey(r.URL.Path)
	if bucket == "" || key != "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "notification is a bucket-level sub-resource; path must be /{bucket}?notification", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket notifications are not configured on this gateway", r.URL.Path)
		return
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeBodyReadError(w, r, err)
		return
	}
	cfg := notification.Config{}
	// An empty body is a valid "clear configuration" request; only
	// attempt to parse when there is a document.
	if len(raw) > 0 {
		var doc notificationConfiguration
		if err := xml.Unmarshal(raw, &doc); err != nil {
			writeError(w, http.StatusBadRequest, "MalformedXML", "could not parse NotificationConfiguration: "+err.Error(), r.URL.Path)
			return
		}
		cfg, err = notificationConfigFromXML(doc)
		if err != nil {
			writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), r.URL.Path)
			return
		}
	}
	if err := cfg.Valid(); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedXML", err.Error(), r.URL.Path)
		return
	}
	if err := h.cfg.BucketConfig.SetNotification(r.Context(), tenantID, bucket, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "NotificationPutFailed", err.Error(), r.URL.Path)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// GetBucketNotificationConfiguration handles GET /{bucket}?notification.
// It returns the bucket's notification configuration. A bucket with no
// configuration returns an empty <NotificationConfiguration/> document
// with 200, matching AWS (there is no NoSuch* error for notifications).
func (h *Handler) GetBucketNotificationConfiguration(w http.ResponseWriter, r *http.Request, bucket string) {
	tenantID, err := h.authenticate(r)
	if err != nil {
		writeAuthError(w, r, err)
		return
	}
	if bucket == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "path must be /{bucket}?notification", r.URL.Path)
		return
	}
	if h.cfg.BucketConfig == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "bucket notifications are not configured on this gateway", r.URL.Path)
		return
	}
	cfg, err := h.cfg.BucketConfig.GetNotification(r.Context(), tenantID, bucket)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "NotificationGetFailed", err.Error(), r.URL.Path)
		return
	}
	writeXMLDoc(w, notificationConfigToXML(cfg))
}
