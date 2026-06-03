package object_lock

import (
	"testing"
	"time"
)

func TestRetentionMode_Valid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode RetentionMode
		want bool
	}{
		{ModeGovernance, true},
		{ModeCompliance, true},
		{RetentionMode(""), false},
		{RetentionMode("governance"), false}, // case-sensitive
		{RetentionMode("BOGUS"), false},
	} {
		if got := tc.mode.Valid(); got != tc.want {
			t.Errorf("%q.Valid() = %v, want %v", tc.mode, got, tc.want)
		}
	}
}

func TestLegalHoldStatus_Valid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s    LegalHoldStatus
		want bool
	}{
		{LegalHoldOn, true},
		{LegalHoldOff, true},
		{LegalHoldStatus(""), false},
		{LegalHoldStatus("on"), false},
	} {
		if got := tc.s.Valid(); got != tc.want {
			t.Errorf("%q.Valid() = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestConfig_Valid(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"disabled empty", Config{Enabled: false}, false},
		{"disabled with stray rule", Config{Enabled: false, DefaultDays: 5}, true},
		{"enabled no rule", Config{Enabled: true}, false},
		{"enabled days rule", Config{Enabled: true, DefaultMode: ModeGovernance, DefaultDays: 30}, false},
		{"enabled years rule", Config{Enabled: true, DefaultMode: ModeCompliance, DefaultYears: 1}, false},
		{"enabled bad mode", Config{Enabled: true, DefaultMode: RetentionMode("x"), DefaultDays: 1}, true},
		{"enabled both days and years", Config{Enabled: true, DefaultMode: ModeGovernance, DefaultDays: 1, DefaultYears: 1}, true},
		{"enabled mode no period", Config{Enabled: true, DefaultMode: ModeGovernance}, true},
		{"enabled negative days", Config{Enabled: true, DefaultMode: ModeGovernance, DefaultDays: -1}, true},
	} {
		err := tc.cfg.Valid()
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: Valid() err = %v, wantErr %v", tc.name, err, tc.wantErr)
		}
	}
}

func TestConfig_HasDefaultRetention(t *testing.T) {
	t.Parallel()
	if (Config{Enabled: true}).HasDefaultRetention() {
		t.Error("enabled-no-rule should not have default retention")
	}
	if (Config{Enabled: false, DefaultMode: ModeGovernance, DefaultDays: 5}).HasDefaultRetention() {
		t.Error("disabled config should not have default retention")
	}
	if !(Config{Enabled: true, DefaultMode: ModeGovernance, DefaultDays: 5}).HasDefaultRetention() {
		t.Error("enabled days rule should have default retention")
	}
}

func TestConfig_DefaultRetainUntil(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := (Config{Enabled: true, DefaultMode: ModeGovernance, DefaultDays: 10}).DefaultRetainUntil(now); !got.Equal(now.AddDate(0, 0, 10)) {
		t.Errorf("days: got %v", got)
	}
	// Years are calendar years (now + N years), matching the AWS SDKs
	// and MinIO, not 365-day spans.
	if got := (Config{Enabled: true, DefaultMode: ModeCompliance, DefaultYears: 2}).DefaultRetainUntil(now); !got.Equal(now.AddDate(2, 0, 0)) {
		t.Errorf("years: got %v", got)
	}
	// A year spanning Feb-29 is a full calendar year (366 days), not 365.
	leapNow := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := (Config{Enabled: true, DefaultMode: ModeCompliance, DefaultYears: 1}).DefaultRetainUntil(leapNow); !got.Equal(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("leap year: got %v, want 2025-01-01", got)
	}
}

func TestRetention_Active(t *testing.T) {
	t.Parallel()
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	future := Retention{Mode: ModeGovernance, RetainUntil: now.Add(time.Hour)}
	if !future.Active(now) {
		t.Error("future retain-until should be active")
	}
	past := Retention{Mode: ModeGovernance, RetainUntil: now.Add(-time.Hour)}
	if past.Active(now) {
		t.Error("expired retain-until should be inactive")
	}
	// No mode means no retention regardless of date.
	none := Retention{RetainUntil: now.Add(time.Hour)}
	if none.Active(now) {
		t.Error("retention with empty mode should be inactive")
	}
}

func TestRetention_Valid(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if err := (Retention{Mode: ModeGovernance, RetainUntil: now}).Valid(); err != nil {
		t.Errorf("valid retention rejected: %v", err)
	}
	if err := (Retention{RetainUntil: now}).Valid(); err == nil {
		t.Error("missing mode should be invalid")
	}
	if err := (Retention{Mode: ModeGovernance}).Valid(); err == nil {
		t.Error("zero RetainUntil should be invalid")
	}
}
