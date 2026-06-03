package cors

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfig_Valid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "empty config rejected",
			cfg:     Config{},
			wantErr: true,
		},
		{
			name: "minimal valid rule",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"https://app.example.com"},
				AllowedMethods: []string{"GET"},
			}}},
		},
		{
			name: "rule missing origin",
			cfg: Config{Rules: []Rule{{
				AllowedMethods: []string{"GET"},
			}}},
			wantErr: true,
		},
		{
			name: "rule missing method",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"*"},
			}}},
			wantErr: true,
		},
		{
			name: "invalid method",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"PATCH"},
			}}},
			wantErr: true,
		},
		{
			name: "empty origin string",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{""},
				AllowedMethods: []string{"GET"},
			}}},
			wantErr: true,
		},
		{
			name: "two wildcards in origin",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"https://*.*.example.com"},
				AllowedMethods: []string{"GET"},
			}}},
			wantErr: true,
		},
		{
			name: "two wildcards in header",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET"},
				AllowedHeaders: []string{"x-**"},
			}}},
			wantErr: true,
		},
		{
			name: "negative max age",
			cfg: Config{Rules: []Rule{{
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET"},
				MaxAgeSeconds:  -1,
			}}},
			wantErr: true,
		},
		{
			name: "ID too long",
			cfg: Config{Rules: []Rule{{
				ID:             strings.Repeat("a", 256),
				AllowedOrigins: []string{"*"},
				AllowedMethods: []string{"GET"},
			}}},
			wantErr: true,
		},
		{
			name:    "too many rules",
			cfg:     Config{Rules: makeRules(maxRules + 1)},
			wantErr: true,
		},
		{
			name: "max rules exactly",
			cfg:  Config{Rules: makeRules(maxRules)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cfg.Valid()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Valid() err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func makeRules(n int) []Rule {
	rules := make([]Rule, n)
	for i := range rules {
		rules[i] = Rule{AllowedOrigins: []string{"*"}, AllowedMethods: []string{"GET"}}
	}
	return rules
}

func TestConfig_Empty(t *testing.T) {
	t.Parallel()
	if !(Config{}).Empty() {
		t.Fatal("zero Config should be Empty")
	}
	if (Config{Rules: []Rule{{}}}).Empty() {
		t.Fatal("Config with a rule should not be Empty")
	}
}

func TestConfig_Match(t *testing.T) {
	t.Parallel()
	cfg := Config{Rules: []Rule{
		{
			AllowedOrigins: []string{"https://only-get.example.com"},
			AllowedMethods: []string{"GET"},
		},
		{
			AllowedOrigins: []string{"https://*.example.com"},
			AllowedMethods: []string{"GET", "PUT"},
		},
		{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"HEAD"},
		},
	}}

	cases := []struct {
		name      string
		origin    string
		method    string
		wantMatch bool
		wantIdx   int // index of expected rule when matched
	}{
		{"exact origin and method", "https://only-get.example.com", "GET", true, 0},
		{"exact origin but method allowed by no rule", "https://only-get.example.com", "DELETE", false, -1},
		{"wildcard subdomain PUT", "https://app.example.com", "PUT", true, 1},
		{"wildcard catch-all HEAD", "https://anything.test", "HEAD", true, 2},
		{"empty origin never matches", "", "GET", false, -1},
		{"case-sensitive suffix mismatch", "https://APP.EXAMPLE.COM", "PUT", false, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rule, ok := cfg.Match(tc.origin, tc.method)
			if ok != tc.wantMatch {
				t.Fatalf("Match(%q,%q) ok = %v, want %v", tc.origin, tc.method, ok, tc.wantMatch)
			}
			if ok && rule.AllowedMethodsCSV() != cfg.Rules[tc.wantIdx].AllowedMethodsCSV() {
				t.Fatalf("matched wrong rule: got methods %q", rule.AllowedMethodsCSV())
			}
		})
	}
}

func TestRule_AllowsHeaders(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		allowed   []string
		requested []string
		want      bool
	}{
		{"no requested headers always allowed", nil, nil, true},
		{"empty entries ignored", []string{"x-amz-meta-foo"}, []string{"", "  "}, true},
		{"exact case-insensitive match", []string{"Content-Type"}, []string{"content-type"}, true},
		{"wildcard allows all", []string{"*"}, []string{"x-amz-meta-anything", "authorization"}, true},
		{"prefix wildcard", []string{"x-amz-*"}, []string{"x-amz-meta-foo"}, true},
		{"prefix wildcard miss", []string{"x-amz-*"}, []string{"x-goog-meta-foo"}, false},
		{"one disallowed rejects all", []string{"content-type"}, []string{"content-type", "authorization"}, false},
		{"no allowed headers rejects requested", nil, []string{"content-type"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := Rule{AllowedHeaders: tc.allowed}
			if got := r.AllowsHeaders(tc.requested); got != tc.want {
				t.Fatalf("AllowsHeaders(%v) = %v, want %v", tc.requested, got, tc.want)
			}
		})
	}
}

func TestRule_CSVHelpers(t *testing.T) {
	t.Parallel()
	r := Rule{AllowedMethods: []string{"GET", "PUT"}, ExposeHeaders: []string{"ETag", "x-amz-version-id"}}
	if got := r.AllowedMethodsCSV(); got != "GET, PUT" {
		t.Fatalf("AllowedMethodsCSV = %q", got)
	}
	if got := r.ExposeHeadersCSV(); got != "ETag, x-amz-version-id" {
		t.Fatalf("ExposeHeadersCSV = %q", got)
	}
	if got := (Rule{}).ExposeHeadersCSV(); got != "" {
		t.Fatalf("empty ExposeHeadersCSV = %q, want empty", got)
	}
}

func TestWildcardMatch(t *testing.T) {
	t.Parallel()
	cases := []struct {
		pattern string
		s       string
		want    bool
	}{
		{"*", "anything", true},
		{"*", "", true},
		{"https://app.example.com", "https://app.example.com", true},
		{"https://app.example.com", "https://other.example.com", false},
		{"https://*.example.com", "https://app.example.com", true},
		{"https://*.example.com", "https://a.b.example.com", true},
		{"https://*.example.com", "https://example.com", false},
		{"pre*suf", "presuf", true},
		{"pre*suf", "preXsuf", true},
		{"pre*suf", "presu", false},
	}
	for _, tc := range cases {
		if got := wildcardMatch(tc.pattern, tc.s); got != tc.want {
			t.Errorf("wildcardMatch(%q,%q) = %v, want %v", tc.pattern, tc.s, got, tc.want)
		}
	}
}

func TestConfig_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	cfg := Config{Rules: []Rule{
		{
			ID:             "rule-1",
			AllowedOrigins: []string{"https://app.example.com", "https://*.cdn.example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  3000,
		},
	}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(got.Rules))
	}
	r := got.Rules[0]
	if r.ID != "rule-1" || r.MaxAgeSeconds != 3000 ||
		strings.Join(r.AllowedOrigins, ",") != "https://app.example.com,https://*.cdn.example.com" ||
		strings.Join(r.AllowedMethods, ",") != "GET,PUT" ||
		strings.Join(r.AllowedHeaders, ",") != "*" ||
		strings.Join(r.ExposeHeaders, ",") != "ETag" {
		t.Fatalf("round-trip mismatch: %+v", r)
	}
}

func TestConfig_JSONEmpty(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(Config{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got.Empty() {
		t.Fatalf("empty config did not round-trip empty: %+v", got)
	}
}
