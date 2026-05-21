package config

import (
	"crypto/tls"
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

func TestRebalancerConfig_EnabledDefaultsToTrueWhenOmitted(t *testing.T) {
	in := []byte(`{"rebalancer": {"targets": [{"tenant_id": "t1", "bucket": "b1", "source_backend": "wasabi", "primary_backend": "ceph_rgw"}]}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !cfg.Rebalancer.Enabled {
		t.Fatalf("Enabled omitted: got false, want true (legacy configs must keep running the rebalancer)")
	}
	if len(cfg.Rebalancer.Targets) != 1 {
		t.Fatalf("Targets = %d, want 1", len(cfg.Rebalancer.Targets))
	}
}

func TestRebalancerConfig_EnabledExplicitFalseDisables(t *testing.T) {
	in := []byte(`{"rebalancer": {"enabled": false, "targets": [{"tenant_id": "t1", "bucket": "b1", "source_backend": "wasabi", "primary_backend": "ceph_rgw"}]}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Rebalancer.Enabled {
		t.Fatalf("Enabled explicit false: got true, want false")
	}
}

func TestConfig_LegacyMigrationKeyFallsBackToRebalancer(t *testing.T) {
	// Legacy configs that predate the rename must still populate
	// Rebalancer without any change on the operator side.
	in := []byte(`{"migration": {"bytes_per_second": 1048576, "interval": "10m", "targets": [{"tenant_id": "t1", "bucket": "b1", "source_backend": "wasabi", "primary_backend": "ceph_rgw"}]}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !cfg.Rebalancer.Enabled {
		t.Fatalf("legacy migration key: Enabled = false, want true")
	}
	if cfg.Rebalancer.BytesPerSecond != 1048576 {
		t.Fatalf("BytesPerSecond = %d, want 1048576", cfg.Rebalancer.BytesPerSecond)
	}
	if cfg.Rebalancer.Interval.ToDuration() != 10*time.Minute {
		t.Fatalf("Interval = %v, want 10m", cfg.Rebalancer.Interval.ToDuration())
	}
	if len(cfg.Rebalancer.Targets) != 1 || cfg.Rebalancer.Targets[0].TenantID != "t1" {
		t.Fatalf("Targets = %+v, want [{TenantID:t1 ...}]", cfg.Rebalancer.Targets)
	}
}

func TestConfig_RebalancerKeyWinsOverLegacyMigration(t *testing.T) {
	// When both keys are present the canonical "rebalancer" wins.
	in := []byte(`{
		"migration":  {"bytes_per_second": 111, "targets": [{"tenant_id": "legacy"}]},
		"rebalancer": {"bytes_per_second": 222, "targets": [{"tenant_id": "canonical"}]}
	}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Rebalancer.BytesPerSecond != 222 {
		t.Fatalf("BytesPerSecond = %d, want 222 (rebalancer wins)", cfg.Rebalancer.BytesPerSecond)
	}
	if len(cfg.Rebalancer.Targets) != 1 || cfg.Rebalancer.Targets[0].TenantID != "canonical" {
		t.Fatalf("Targets = %+v, want canonical tenant", cfg.Rebalancer.Targets)
	}
}

func TestGatewayConfig_JSONRejectsBareNumberTimeout(t *testing.T) {
	in := []byte(`{"gateway": {"read_timeout": 30}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err == nil {
		t.Fatalf("Unmarshal bare number: want error, got nil (ReadTimeout=%v)", cfg.Gateway.ReadTimeout.ToDuration())
	}
}

func TestTLSConfig_Enabled(t *testing.T) {
	cases := []struct {
		name string
		in   TLSConfig
		want bool
	}{
		{"both empty", TLSConfig{}, false},
		{"cert only", TLSConfig{CertPath: "/etc/tls/cert.pem"}, false},
		{"key only", TLSConfig{KeyPath: "/etc/tls/key.pem"}, false},
		{"both set", TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTLSConfig_MinTLSVersion(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    uint16
		wantErr bool
	}{
		{"empty defaults to 1.2", "", tls.VersionTLS12, false},
		{"1.2 short form", "1.2", tls.VersionTLS12, false},
		{"1.3 short form", "1.3", tls.VersionTLS13, false},
		{"tls1.2 long form", "tls1.2", tls.VersionTLS12, false},
		{"tls1.3 long form", "tls1.3", tls.VersionTLS13, false},
		{"case insensitive", "TLS1.3", tls.VersionTLS13, false},
		{"with whitespace", "  1.3  ", tls.VersionTLS13, false},
		{"1.1 rejected", "1.1", 0, true},
		{"1.0 rejected", "1.0", 0, true},
		{"garbage rejected", "yes please", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := TLSConfig{MinVersion: tc.in}
			got, err := c.MinTLSVersion()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("MinTLSVersion(%q): want error, got %#x", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MinTLSVersion(%q): %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("MinTLSVersion(%q) = %#x, want %#x", tc.in, got, tc.want)
			}
		})
	}
}

func TestTLSConfig_Validate(t *testing.T) {
	cases := []struct {
		name      string
		in        TLSConfig
		wantErr   bool
		wantMatch string // substring the error must contain
	}{
		{"both empty is valid (plain HTTP)", TLSConfig{}, false, ""},
		{"both set is valid (HTTPS)", TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem"}, false, ""},
		{"both set with 1.3 is valid", TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem", MinVersion: "1.3"}, false, ""},
		{"cert only is invalid", TLSConfig{CertPath: "/etc/tls/cert.pem"}, true, "cert_path is set but key_path is empty"},
		{"key only is invalid", TLSConfig{KeyPath: "/etc/tls/key.pem"}, true, "key_path is set but cert_path is empty"},
		{"unrecognised min_version is invalid", TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem", MinVersion: "1.0"}, true, "min_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.in.Validate("gateway")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate: want error, got nil")
				}
				if tc.wantMatch != "" && !strings.Contains(err.Error(), tc.wantMatch) {
					t.Fatalf("Validate error = %q; want substring %q", err.Error(), tc.wantMatch)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: unexpected error %v", err)
			}
		})
	}
}

func TestTLSConfig_BuildGoTLSConfig(t *testing.T) {
	t.Run("default min version is 1.2", func(t *testing.T) {
		c := TLSConfig{}
		got, err := c.BuildGoTLSConfig()
		if err != nil {
			t.Fatalf("BuildGoTLSConfig: %v", err)
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Fatalf("MinVersion = %#x, want %#x", got.MinVersion, tls.VersionTLS12)
		}
		// PreferServerCipherSuites is intentionally not set
		// (deprecated in Go 1.18). The fixed cipher order
		// crypto/tls uses is verified indirectly by the
		// TLS handshake roundtrip test in cmd/gateway.
	})
	t.Run("explicit 1.3", func(t *testing.T) {
		c := TLSConfig{MinVersion: "1.3"}
		got, err := c.BuildGoTLSConfig()
		if err != nil {
			t.Fatalf("BuildGoTLSConfig: %v", err)
		}
		if got.MinVersion != tls.VersionTLS13 {
			t.Fatalf("MinVersion = %#x, want %#x", got.MinVersion, tls.VersionTLS13)
		}
	})
	t.Run("invalid min version", func(t *testing.T) {
		c := TLSConfig{MinVersion: "1.0"}
		if _, err := c.BuildGoTLSConfig(); err == nil {
			t.Fatal("BuildGoTLSConfig: want error, got nil")
		}
	})
}

func TestTLSConfig_JSONRoundTrip(t *testing.T) {
	in := []byte(`{
		"gateway": {
			"tls": {"cert_path": "/etc/tls/cert.pem", "key_path": "/etc/tls/key.pem", "min_version": "1.3"}
		},
		"console": {
			"tls": {"cert_path": "/etc/console/cert.pem", "key_path": "/etc/console/key.pem"}
		},
		"health": {
			"tls": {"cert_path": "/etc/health/cert.pem", "key_path": "/etc/health/key.pem"}
		}
	}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if cfg.Gateway.TLS.CertPath != "/etc/tls/cert.pem" || cfg.Gateway.TLS.KeyPath != "/etc/tls/key.pem" || cfg.Gateway.TLS.MinVersion != "1.3" {
		t.Fatalf("Gateway.TLS = %+v", cfg.Gateway.TLS)
	}
	if !cfg.Gateway.TLS.Enabled() {
		t.Fatal("Gateway.TLS.Enabled() = false, want true")
	}
	if cfg.Console.TLS.CertPath != "/etc/console/cert.pem" || !cfg.Console.TLS.Enabled() {
		t.Fatalf("Console.TLS = %+v", cfg.Console.TLS)
	}
	if cfg.Health.TLS.CertPath != "/etc/health/cert.pem" || !cfg.Health.TLS.Enabled() {
		t.Fatalf("Health.TLS = %+v", cfg.Health.TLS)
	}
}
