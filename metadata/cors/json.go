package cors

import "encoding/json"

// jsonRule is the on-disk JSON shape of a Rule. It is intentionally
// separate from Rule so the persisted format is decoupled from the
// in-memory struct: renaming a Rule field never silently changes what
// is already stored. The lowercase tags keep stored documents compact
// and stable across versions.
type jsonRule struct {
	ID             string   `json:"id,omitempty"`
	AllowedOrigins []string `json:"allowed_origins,omitempty"`
	AllowedMethods []string `json:"allowed_methods,omitempty"`
	AllowedHeaders []string `json:"allowed_headers,omitempty"`
	ExposeHeaders  []string `json:"expose_headers,omitempty"`
	MaxAgeSeconds  int      `json:"max_age_seconds,omitempty"`
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
		//lint:ignore S1016 intentional decoupling of on-disk and in-memory shapes
		out.Rules[i] = jsonRule{
			ID:             r.ID,
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAgeSeconds:  r.MaxAgeSeconds,
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
		//lint:ignore S1016 intentional decoupling of on-disk and in-memory shapes
		rules[i] = Rule{
			ID:             r.ID,
			AllowedOrigins: r.AllowedOrigins,
			AllowedMethods: r.AllowedMethods,
			AllowedHeaders: r.AllowedHeaders,
			ExposeHeaders:  r.ExposeHeaders,
			MaxAgeSeconds:  r.MaxAgeSeconds,
		}
	}
	c.Rules = rules
	return nil
}
