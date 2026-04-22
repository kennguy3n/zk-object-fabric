package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDuration_UnmarshalJSON_AcceptsString(t *testing.T) {
	cases := map[string]time.Duration{
		`"30s"`:   30 * time.Second,
		`"5m"`:    5 * time.Minute,
		`"250ms"`: 250 * time.Millisecond,
		`"1h30m"`: 90 * time.Minute,
	}
	for input, want := range cases {
		t.Run(input, func(t *testing.T) {
			var d Duration
			if err := json.Unmarshal([]byte(input), &d); err != nil {
				t.Fatalf("Unmarshal(%s): %v", input, err)
			}
			if d.ToDuration() != want {
				t.Fatalf("Unmarshal(%s) = %v, want %v", input, d.ToDuration(), want)
			}
		})
	}
}

func TestDuration_UnmarshalJSON_RejectsBareNumbers(t *testing.T) {
	cases := []string{`30`, `30.5`, `0`}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			var d Duration
			err := json.Unmarshal([]byte(input), &d)
			if err == nil {
				t.Fatalf("Unmarshal(%s): want error, got nil (value = %v)", input, d.ToDuration())
			}
			if !strings.Contains(err.Error(), "quoted string") {
				t.Fatalf("Unmarshal(%s) error = %q, want to mention quoted string", input, err)
			}
		})
	}
}

func TestDuration_UnmarshalJSON_RejectsInvalidString(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"not-a-duration"`), &d); err == nil {
		t.Fatal("Unmarshal(\"not-a-duration\"): want error, got nil")
	}
}

func TestDuration_MarshalJSON_RoundTrip(t *testing.T) {
	orig := Duration(45 * time.Second)
	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `"45s"` {
		t.Fatalf("Marshal = %s, want \"45s\"", b)
	}
	var back Duration
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back != orig {
		t.Fatalf("round-trip mismatch: got %v, want %v", back, orig)
	}
}

func TestGatewayConfig_JSONUsesStringDurations(t *testing.T) {
	in := []byte(`{"gateway": {"listen_addr": ":9090", "read_timeout": "15s", "write_timeout": "45s"}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal config: %v", err)
	}
	if cfg.Gateway.ReadTimeout.ToDuration() != 15*time.Second {
		t.Fatalf("ReadTimeout = %v, want 15s", cfg.Gateway.ReadTimeout.ToDuration())
	}
	if cfg.Gateway.WriteTimeout.ToDuration() != 45*time.Second {
		t.Fatalf("WriteTimeout = %v, want 45s", cfg.Gateway.WriteTimeout.ToDuration())
	}
}

func TestGatewayConfig_JSONRejectsBareNumberTimeout(t *testing.T) {
	in := []byte(`{"gateway": {"read_timeout": 30}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err == nil {
		t.Fatalf("Unmarshal bare number: want error, got nil (ReadTimeout=%v)", cfg.Gateway.ReadTimeout.ToDuration())
	}
}
