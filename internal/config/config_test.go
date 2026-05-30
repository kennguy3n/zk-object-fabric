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

// TestTLSConfig_Validate_MinVersionErrorHasSinglePrefix is a
// regression guard against the double "config:" prefix bug.
//
// Pre-fix: MinTLSVersion returned `config: tls.min_version "1.0":
// must be …` and Validate wrapped it as `config: %s.tls: %w`,
// producing `config: gateway.tls: config: tls.min_version "1.0":
// must be …` — two "config:" tokens surfacing to operators at
// startup.
//
// Post-fix: MinTLSVersion returns `tls.min_version "1.0": must
// be …` (no embedded prefix), and Validate adds the single
// `config: gateway.tls:` prefix. The full message is
// `config: gateway.tls: tls.min_version "1.0": must be …`.
//
// This test asserts the post-fix shape by pinning the substring
// "config: gateway.tls: tls.min_version" — if a future change
// reintroduces "config:" into the leaf or duplicates the wrap,
// the test fails.
func TestTLSConfig_Validate_MinVersionErrorHasSinglePrefix(t *testing.T) {
	in := TLSConfig{
		CertPath:   "/etc/tls/cert.pem",
		KeyPath:    "/etc/tls/key.pem",
		MinVersion: "1.0",
	}
	err := in.Validate("gateway")
	if err == nil {
		t.Fatal("Validate: want error for min_version 1.0, got nil")
	}
	msg := err.Error()
	// Exactly one "config:" prefix at the start of the message.
	if !strings.HasPrefix(msg, "config: gateway.tls: tls.min_version ") {
		t.Errorf("Validate error = %q; want prefix %q", msg, "config: gateway.tls: tls.min_version ")
	}
	if strings.Count(msg, "config:") != 1 {
		t.Errorf("Validate error has %d occurrences of \"config:\", want exactly 1; full message: %q", strings.Count(msg, "config:"), msg)
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

// TestDefault_ControlPlanePoolDefaults pins the connection-pool
// ceilings Default() applies to ControlPlaneConfig. Operators who
// run with config.control_plane unset (e.g. dev / staging without
// a bespoke config file) inherit these values; the test guards
// against an accidental refactor that drops one of them and falls
// back to Go's stdlib defaults (unlimited MaxOpenConns,
// MaxIdleConns=2, no lifetime) which would silently saturate RDS.
func TestDefault_ControlPlanePoolDefaults(t *testing.T) {
	d := Default().ControlPlane
	if d.MaxOpenConns != 32 {
		t.Errorf("MaxOpenConns = %d, want 32", d.MaxOpenConns)
	}
	if d.MaxIdleConns != 8 {
		t.Errorf("MaxIdleConns = %d, want 8", d.MaxIdleConns)
	}
	if got := d.ConnMaxLifetime.ToDuration(); got != 4*time.Minute {
		t.Errorf("ConnMaxLifetime = %v, want 4m", got)
	}
	if got := d.ConnMaxIdleTime.ToDuration(); got != 2*time.Minute {
		t.Errorf("ConnMaxIdleTime = %v, want 2m", got)
	}
}

func TestGatewayConfig_CacheWarmingMemoryBudget_DefaultApplied(t *testing.T) {
	cfg := Default()
	if got := cfg.Gateway.CacheWarmingMemoryBudget; got != 512*1024*1024 {
		t.Fatalf("Default CacheWarmingMemoryBudget = %d, want 512 MiB", got)
	}
}

func TestGatewayConfig_CacheWarmingMemoryBudget_OverrideFromJSON(t *testing.T) {
	in := []byte(`{"gateway": {"cache_warming_memory_budget": 1073741824}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := cfg.Gateway.CacheWarmingMemoryBudget; got != 1024*1024*1024 {
		t.Fatalf("CacheWarmingMemoryBudget = %d, want 1 GiB", got)
	}
}

func TestGatewayConfig_CacheWarmingMemoryBudget_NegativeDisablesGuard(t *testing.T) {
	// A negative budget is the documented opt-out for the budget
	// guard; the field is honoured verbatim so the s3compat
	// Handler can detect "disabled" via budget < 0.
	in := []byte(`{"gateway": {"cache_warming_memory_budget": -1}}`)
	var cfg Config
	if err := json.Unmarshal(in, &cfg); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := cfg.Gateway.CacheWarmingMemoryBudget; got != -1 {
		t.Fatalf("CacheWarmingMemoryBudget = %d, want -1 (disabled)", got)
	}
}

// TestConfig_Validate_RejectsHeaderTimeoutGreaterThanRead pins the
// cross-field check that surfaces the Slowloris-defence footgun:
// when ReadHeaderTimeout > ReadTimeout, ReadTimeout (which bounds
// the entire request lifecycle including headers and body) fires
// first and the cheaper header-stall timeout is never reached.
// Devin Review on PR #80 flagged this as an unguarded operational
// pitfall; this test pins the guard so a future refactor that
// drops the check fails loudly here rather than in production.
func TestConfig_Validate_RejectsHeaderTimeoutGreaterThanRead(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutator func(c *Config)
		wantErr string
	}{
		{
			name: "gateway",
			mutator: func(c *Config) {
				c.Gateway.ReadTimeout = Duration(30 * time.Second)
				c.Gateway.ReadHeaderTimeout = Duration(60 * time.Second)
			},
			wantErr: "gateway.read_header_timeout",
		},
		{
			name: "console",
			mutator: func(c *Config) {
				c.Console.ReadTimeout = Duration(15 * time.Second)
				c.Console.ReadHeaderTimeout = Duration(20 * time.Second)
			},
			wantErr: "console.read_header_timeout",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutator(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("Validate returned nil, want error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

// TestConfig_Validate_AcceptsDefault locks in the invariant that
// Default() returns a Config that Validate() accepts. A future
// change that lowers the gateway ReadTimeout below the
// ReadHeaderTimeout default (10s) would break this test before
// any deployment does the same thing accidentally.
func TestConfig_Validate_AcceptsDefault(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default() failed Validate: %v", err)
	}
}

// TestConfig_Validate_EqualHeaderAndReadTimeoutsAllowed allows the
// operator to set ReadHeaderTimeout == ReadTimeout — there is no
// silent nullification at the boundary (both fire at the same
// moment) and an operator who pins them equal has explicitly
// chosen a single timeout window. The guard is a strict
// less-than-or-equal check, not a less-than check.
func TestConfig_Validate_EqualHeaderAndReadTimeoutsAllowed(t *testing.T) {
	cfg := Default()
	cfg.Gateway.ReadTimeout = Duration(30 * time.Second)
	cfg.Gateway.ReadHeaderTimeout = Duration(30 * time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected equal timeouts: %v", err)
	}
}

// TestConfig_Validate_ZeroTimeoutSkipsCheck pins that the guard
// no-ops when either ReadTimeout or ReadHeaderTimeout is zero so
// deployments that intentionally leave one unset (e.g. only
// ReadTimeout configured, ReadHeaderTimeout left to Go's
// default) are not blocked.
func TestConfig_Validate_ZeroTimeoutSkipsCheck(t *testing.T) {
	cfg := Default()
	cfg.Gateway.ReadTimeout = 0
	cfg.Gateway.ReadHeaderTimeout = Duration(60 * time.Second)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected zero ReadTimeout: %v", err)
	}
}
