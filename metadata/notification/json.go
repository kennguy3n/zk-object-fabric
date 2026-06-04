package notification

import "encoding/json"

// jsonRule is the on-disk JSON shape of a Rule. It is intentionally
// separate from Rule so the persisted format is decoupled from the
// in-memory struct: renaming a Rule field never silently changes what
// is already stored. The lowercase tags keep stored documents compact
// and stable across versions.
type jsonRule struct {
	ID       string   `json:"id,omitempty"`
	Events   []string `json:"events,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Prefix   string   `json:"prefix,omitempty"`
	Suffix   string   `json:"suffix,omitempty"`
}

type jsonConfig struct {
	Rules []jsonRule `json:"rules,omitempty"`
}

// MarshalJSON serialises the configuration to the stable persistence
// format used by the Postgres and SQLite stores.
func (c Config) MarshalJSON() ([]byte, error) {
	out := jsonConfig{Rules: make([]jsonRule, len(c.Rules))}
	for i, r := range c.Rules {
		// Explicit field mapping (not a jsonRule(r) conversion) so the
		// persisted format stays decoupled from Rule: a field added to
		// Rule must be deliberately threaded through here rather than
		// silently appearing on disk.
		events := make([]string, len(r.Events))
		for j, e := range r.Events {
			events[j] = string(e)
		}
		out.Rules[i] = jsonRule{
			ID:       r.ID,
			Events:   events,
			Endpoint: r.Endpoint,
			Prefix:   r.Prefix,
			Suffix:   r.Suffix,
		}
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
	for i, r := range in.Rules {
		events := make([]EventType, len(r.Events))
		for j, e := range r.Events {
			events[j] = EventType(e)
		}
		rules[i] = Rule{
			ID:       r.ID,
			Events:   events,
			Endpoint: r.Endpoint,
			Prefix:   r.Prefix,
			Suffix:   r.Suffix,
		}
	}
	c.Rules = rules
	return nil
}
