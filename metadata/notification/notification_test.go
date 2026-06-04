package notification

import "testing"

func TestEventCovers(t *testing.T) {
	cases := []struct {
		sub, emitted EventType
		want         bool
	}{
		{ObjectCreatedAll, ObjectCreatedPut, true},
		{ObjectCreatedAll, ObjectCreatedCopy, true},
		{ObjectCreatedAll, ObjectRemovedDelete, false},
		{ObjectRemovedAll, ObjectRemovedDeleteMarkerCreated, true},
		{ObjectCreatedPut, ObjectCreatedPut, true},
		{ObjectCreatedPut, ObjectCreatedCopy, false},
		{ObjectRemovedAll, ObjectCreatedPut, false},
	}
	for _, c := range cases {
		if got := c.sub.covers(c.emitted); got != c.want {
			t.Errorf("%s covers %s = %v, want %v", c.sub, c.emitted, got, c.want)
		}
	}
}

func TestConfigValid(t *testing.T) {
	ok := Config{Rules: []Rule{{
		ID:       "a",
		Events:   []EventType{ObjectCreatedAll},
		Endpoint: "https://example.com/hook",
	}}}
	if err := ok.Valid(); err != nil {
		t.Errorf("valid config rejected: %v", err)
	}

	// Empty config is valid (clears configuration).
	if err := (Config{}).Valid(); err != nil {
		t.Errorf("empty config rejected: %v", err)
	}

	bad := []Config{
		{Rules: []Rule{{Events: []EventType{ObjectCreatedAll}}}},                              // no endpoint
		{Rules: []Rule{{Endpoint: "https://e.com"}}},                                          // no events
		{Rules: []Rule{{Events: []EventType{"s3:Bogus"}, Endpoint: "https://e.com"}}},         // bad event
		{Rules: []Rule{{Events: []EventType{ObjectCreatedAll}, Endpoint: "ftp://e.com"}}},     // bad scheme
		{Rules: []Rule{{Events: []EventType{ObjectCreatedAll}, Endpoint: "https:///nohost"}}}, // no host
		{Rules: []Rule{
			{ID: "dup", Events: []EventType{ObjectCreatedAll}, Endpoint: "https://e.com"},
			{ID: "dup", Events: []EventType{ObjectRemovedAll}, Endpoint: "https://e.com"},
		}}, // duplicate IDs
	}
	for i, c := range bad {
		if err := c.Valid(); err == nil {
			t.Errorf("bad config %d accepted", i)
		}
	}
}

func TestConfigMatch(t *testing.T) {
	cfg := Config{Rules: []Rule{
		{ID: "creates", Events: []EventType{ObjectCreatedAll}, Endpoint: "https://e.com/c"},
		{ID: "logs-json", Events: []EventType{ObjectCreatedPut}, Endpoint: "https://e.com/l", Prefix: "logs/", Suffix: ".json"},
		{ID: "removes", Events: []EventType{ObjectRemovedAll}, Endpoint: "https://e.com/r"},
	}}

	// A create of logs/app.json matches both the wildcard-create rule
	// and the filtered rule.
	got := cfg.Match(ObjectCreatedPut, "logs/app.json")
	if len(got) != 2 || got[0].ID != "creates" || got[1].ID != "logs-json" {
		t.Fatalf("create logs/app.json matched %v", ruleIDs(got))
	}

	// A create that fails the suffix filter only matches the wildcard.
	got = cfg.Match(ObjectCreatedPut, "logs/app.txt")
	if len(got) != 1 || got[0].ID != "creates" {
		t.Fatalf("create logs/app.txt matched %v", ruleIDs(got))
	}

	// A delete matches only the removes rule.
	got = cfg.Match(ObjectRemovedDelete, "logs/app.json")
	if len(got) != 1 || got[0].ID != "removes" {
		t.Fatalf("delete matched %v", ruleIDs(got))
	}
}

func ruleIDs(rs []Rule) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}
