package lifecycle

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func i64(v int64) *int64 { return &v }

func TestConfigValid(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{
			name:    "empty config",
			cfg:     Config{},
			wantErr: true,
		},
		{
			name: "expiration days",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Expiration: &Expiration{Days: 30},
			}}},
		},
		{
			name: "rule with no action",
			cfg: Config{Rules: []Rule{{
				Status: StatusEnabled,
				Filter: Filter{Prefix: "logs/"},
			}}},
			wantErr: true,
		},
		{
			name: "bad status",
			cfg: Config{Rules: []Rule{{
				Status:     "On",
				Expiration: &Expiration{Days: 1},
			}}},
			wantErr: true,
		},
		{
			name: "expiration days and date both set",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Expiration: &Expiration{Days: 1, Date: time.Now()},
			}}},
			wantErr: true,
		},
		{
			name: "transition to STANDARD is invalid",
			cfg: Config{Rules: []Rule{{
				Status:      StatusEnabled,
				Transitions: []Transition{{Days: 30, StorageClass: "STANDARD"}},
			}}},
			wantErr: true,
		},
		{
			name: "transition to GLACIER ok",
			cfg: Config{Rules: []Rule{{
				Status:      StatusEnabled,
				Transitions: []Transition{{Days: 30, StorageClass: "GLACIER"}},
			}}},
		},
		{
			name: "abort with tag filter rejected",
			cfg: Config{Rules: []Rule{{
				Status:                         StatusEnabled,
				Filter:                         Filter{Tags: map[string]string{"k": "v"}},
				AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
			}}},
			wantErr: true,
		},
		{
			name: "abort days must be positive",
			cfg: Config{Rules: []Rule{{
				Status:                         StatusEnabled,
				AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 0},
			}}},
			wantErr: true,
		},
		{
			name: "size gt must be < size lt",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Filter:     Filter{ObjectSizeGreaterThan: i64(100), ObjectSizeLessThan: i64(50)},
				Expiration: &Expiration{Days: 1},
			}}},
			wantErr: true,
		},
		{
			// AWS rejects ObjectSizeLessThan below 1: "< 0 bytes"
			// matches no object, so it is a useless predicate.
			name: "size lt zero rejected",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Filter:     Filter{ObjectSizeLessThan: i64(0)},
				Expiration: &Expiration{Days: 1},
			}}},
			wantErr: true,
		},
		{
			// ObjectSizeGreaterThan of 0 is valid (selects every
			// object larger than zero bytes).
			name: "size gt zero ok",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Filter:     Filter{ObjectSizeGreaterThan: i64(0)},
				Expiration: &Expiration{Days: 1},
			}}},
		},
		{
			name: "duplicate rule IDs",
			cfg: Config{Rules: []Rule{
				{ID: "r1", Status: StatusEnabled, Expiration: &Expiration{Days: 1}},
				{ID: "r1", Status: StatusEnabled, Expiration: &Expiration{Days: 2}},
			}},
			wantErr: true,
		},
		{
			name: "expired object delete marker ok",
			cfg: Config{Rules: []Rule{{
				Status:     StatusEnabled,
				Expiration: &Expiration{ExpiredObjectDeleteMarker: true},
			}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Valid()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Valid() err=%v, wantErr=%v", err, tt.wantErr)
			}
		})
	}
}

func TestRuleMatches(t *testing.T) {
	r := Rule{
		Status: StatusEnabled,
		Filter: Filter{
			Prefix:                "logs/",
			Tags:                  map[string]string{"archive": "yes"},
			ObjectSizeGreaterThan: i64(10),
			ObjectSizeLessThan:    i64(1000),
		},
		Expiration: &Expiration{Days: 1},
	}
	tags := map[string]string{"archive": "yes", "other": "x"}
	if !r.Matches("logs/app.log", tags, 100) {
		t.Fatal("expected match")
	}
	if r.Matches("data/app.log", tags, 100) {
		t.Fatal("prefix should exclude")
	}
	if r.Matches("logs/app.log", map[string]string{"archive": "no"}, 100) {
		t.Fatal("tag mismatch should exclude")
	}
	if r.Matches("logs/app.log", tags, 10) {
		t.Fatal("size == gt boundary should exclude (strictly greater)")
	}
	if r.Matches("logs/app.log", tags, 1000) {
		t.Fatal("size == lt boundary should exclude (strictly less)")
	}
	// Empty filter matches everything.
	any := Rule{Status: StatusEnabled, Expiration: &Expiration{Days: 1}}
	if !any.Matches("whatever", nil, 0) {
		t.Fatal("empty filter should match all")
	}
}

func TestExpiresAt(t *testing.T) {
	created := time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)

	// Days-based: rounds up to next UTC midnight after created+Days.
	r := Rule{Expiration: &Expiration{Days: 10}}
	at, ok := r.ExpiresAt(created)
	if !ok {
		t.Fatal("expected age-based expiry")
	}
	want := time.Date(2024, 1, 12, 0, 0, 0, 0, time.UTC) // 2024-01-11 10:30 -> next midnight
	if !at.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v", at, want)
	}

	// Zero createdAt => unknown age => never expire (fail safe).
	if _, ok := r.ExpiresAt(time.Time{}); ok {
		t.Fatal("zero createdAt must not expire")
	}

	// Date-based: returns the date verbatim.
	d := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	rd := Rule{Expiration: &Expiration{Date: d}}
	at, ok = rd.ExpiresAt(created)
	if !ok || !at.Equal(d) {
		t.Fatalf("date rule ExpiresAt = %v,%v want %v,true", at, ok, d)
	}

	// ExpiredObjectDeleteMarker is not age-driven.
	rm := Rule{Expiration: &Expiration{ExpiredObjectDeleteMarker: true}}
	if _, ok := rm.ExpiresAt(created); ok {
		t.Fatal("ExpiredObjectDeleteMarker must not be age-driven")
	}

	// No expiration at all.
	if _, ok := (Rule{}).ExpiresAt(created); ok {
		t.Fatal("no expiration => no expiry")
	}
}

func TestExpiresAtExactMidnight(t *testing.T) {
	created := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	r := Rule{Expiration: &Expiration{Days: 5}}
	at, ok := r.ExpiresAt(created)
	if !ok {
		t.Fatal("expected expiry")
	}
	want := time.Date(2024, 1, 6, 0, 0, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Fatalf("ExpiresAt = %v, want %v (already on midnight, no extra day)", at, want)
	}
}

func TestAbortStaleBefore(t *testing.T) {
	now := time.Date(2024, 3, 10, 12, 0, 0, 0, time.UTC)
	r := Rule{AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 7}}
	cutoff, ok := r.AbortStaleBefore(now)
	if !ok {
		t.Fatal("expected abort cutoff")
	}
	want := time.Date(2024, 3, 3, 12, 0, 0, 0, time.UTC)
	if !cutoff.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", cutoff, want)
	}
	if _, ok := (Rule{}).AbortStaleBefore(now); ok {
		t.Fatal("no abort action => no cutoff")
	}
}

func TestEnabled(t *testing.T) {
	if !(Rule{Status: StatusEnabled}).Enabled() {
		t.Fatal("Enabled rule should report enabled")
	}
	if (Rule{Status: StatusDisabled}).Enabled() {
		t.Fatal("Disabled rule should not report enabled")
	}
}

func TestJSONRoundTrip(t *testing.T) {
	date := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := Config{Rules: []Rule{
		{
			ID:     "expire-logs",
			Status: StatusEnabled,
			Filter: Filter{
				Prefix:                "logs/",
				Tags:                  map[string]string{"team": "infra"},
				ObjectSizeGreaterThan: i64(1024),
			},
			Expiration:  &Expiration{Days: 90},
			Transitions: []Transition{{Days: 30, StorageClass: "GLACIER"}},
		},
		{
			ID:                             "abort-mpu",
			Status:                         StatusDisabled,
			Expiration:                     &Expiration{Date: date},
			AbortIncompleteMultipartUpload: &AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
		},
	}}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Config
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(cfg, got) {
		t.Fatalf("round-trip mismatch:\n got=%#v\nwant=%#v", got, cfg)
	}
}

func TestEmpty(t *testing.T) {
	if !(Config{}).Empty() {
		t.Fatal("zero Config should be Empty")
	}
	if (Config{Rules: []Rule{{}}}).Empty() {
		t.Fatal("Config with rules should not be Empty")
	}
	if !(Filter{}).Empty() {
		t.Fatal("zero Filter should be Empty")
	}
	if (Filter{Prefix: "x"}).Empty() {
		t.Fatal("Filter with prefix should not be Empty")
	}
}
