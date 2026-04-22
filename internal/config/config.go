// Package config loads runtime configuration for the gateway and the
// control-plane services. Phase 1 accepts only a minimal set of
// knobs; the file format and schema will expand as Phase 2 lands.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"
)

// Duration is a time.Duration that (un)marshals through JSON using
// the human-readable syntax accepted by time.ParseDuration
// (e.g. "30s", "5m", "250ms"). Bare JSON numbers are rejected to
// avoid the silent nanosecond trap of time.Duration's default
// encoding.
type Duration time.Duration

// String reports the wrapped duration in Go's canonical form.
func (d Duration) String() string { return time.Duration(d).String() }

// ToDuration returns the value as a time.Duration.
func (d Duration) ToDuration() time.Duration { return time.Duration(d) }

// MarshalJSON emits the duration as a quoted string like "30s".
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON accepts quoted duration strings like "30s" and
// rejects bare numbers. Rejecting numbers is intentional:
// time.Duration is int64 nanoseconds, so "read_timeout": 30 would
// silently be 30ns.
func (d *Duration) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || data[0] != '"' {
		if _, err := strconv.ParseFloat(string(data), 64); err == nil {
			return fmt.Errorf("config: duration must be a quoted string like \"30s\"; got bare number %s", string(data))
		}
		return fmt.Errorf("config: duration must be a quoted string like \"30s\"; got %s", string(data))
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("config: duration decode: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("config: parse duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the top-level runtime configuration.
type Config struct {
	Env          string             `json:"env"`
	Gateway      GatewayConfig      `json:"gateway"`
	ControlPlane ControlPlaneConfig `json:"control_plane"`
	Providers    ProvidersConfig    `json:"providers"`
	Rebalancer   RebalancerConfig   `json:"rebalancer"`
	Billing      BillingConfig      `json:"billing"`
	Health       HealthConfig       `json:"health"`
	Console      ConsoleConfig      `json:"console"`
}

// ConsoleConfig configures the tenant-console HTTP surface (api/console).
// The console runs on its own listener so a saturated S3 data plane
// cannot starve the management controls. Leave ListenAddr empty to
// disable the console API entirely.
type ConsoleConfig struct {
	ListenAddr   string   `json:"listen_addr"`
	ReadTimeout  Duration `json:"read_timeout"`
	WriteTimeout Duration `json:"write_timeout"`
}

// UnmarshalJSON accepts both the canonical "rebalancer" key and the
// legacy "migration" key for RebalancerConfig. When both are
// present "rebalancer" wins. This lets operators migrate their
// gateway config files at their own pace.
func (c *Config) UnmarshalJSON(data []byte) error {
	type alias Config
	aux := struct {
		*alias
		Migration *RebalancerConfig `json:"migration,omitempty"`
	}{alias: (*alias)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	// Fall back to the legacy key only when the canonical
	// "rebalancer" block was not supplied.
	if aux.Migration != nil && !rebalancerPresent(data) {
		c.Rebalancer = *aux.Migration
	}
	return nil
}

// rebalancerPresent reports whether the raw JSON object has an
// explicit "rebalancer" key. Used to disambiguate "supplied but
// zero-valued" from "omitted" when picking between the canonical
// and legacy config keys.
func rebalancerPresent(data []byte) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, ok := raw["rebalancer"]
	return ok
}

// GatewayConfig configures the S3-compatible gateway fleet on Linode.
//
// ReadTimeout and WriteTimeout use the local Duration type so that
// human-authored JSON can specify "30s" instead of nanoseconds.
type GatewayConfig struct {
	ListenAddr      string   `json:"listen_addr"`
	ReadTimeout     Duration `json:"read_timeout"`
	WriteTimeout    Duration `json:"write_timeout"`
	MaxRequestBytes int64    `json:"max_request_bytes"`
	CachePath       string   `json:"cache_path"`
}

// ControlPlaneConfig configures the AWS-hosted control plane surface
// the gateway talks to.
type ControlPlaneConfig struct {
	MetadataDSN string `json:"metadata_dsn"`
	AuthIssuer  string `json:"auth_issuer"`
	BillingURL  string `json:"billing_url"`
}

// BillingConfig configures the metering sink. When ClickHouseURL is
// empty the gateway falls back to the development LoggerSink.
type BillingConfig struct {
	ClickHouseURL      string   `json:"clickhouse_url"`
	ClickHouseDatabase string   `json:"clickhouse_database"`
	ClickHouseTable    string   `json:"clickhouse_table"`
	ClickHouseUsername string   `json:"clickhouse_username"`
	ClickHousePassword string   `json:"clickhouse_password"`
	BatchSize          int      `json:"batch_size"`
	FlushInterval      Duration `json:"flush_interval"`
}

// HealthConfig configures the gateway fleet node health monitor.
// When ListenAddr is empty the monitor still runs as a background
// quorum watcher but does not expose HTTP endpoints.
type HealthConfig struct {
	// NodeID identifies this gateway in peer /health responses.
	// Defaults to the hostname.
	NodeID string `json:"node_id"`
	// CellID is the logical cell this gateway belongs to.
	CellID string `json:"cell_id"`
	// ListenAddr is the bind address for the internal health
	// endpoints (e.g. ":29090"). Optional.
	ListenAddr string `json:"listen_addr"`
	// Peers is the cell's peer gateway list.
	Peers []HealthPeer `json:"peers"`
	// QuorumThreshold is the minimum number of nodes (including
	// local) that must be healthy for the cell to be in quorum.
	// Defaults to a simple majority of (peers + 1).
	QuorumThreshold int `json:"quorum_threshold"`
	// PollInterval is the peer poll cadence. Defaults to 2s.
	PollInterval Duration `json:"poll_interval"`
	// PollTimeout bounds one peer probe. Defaults to 1s.
	PollTimeout Duration `json:"poll_timeout"`
	// DrainTimeout bounds the drain wait. Defaults to 30s.
	DrainTimeout Duration `json:"drain_timeout"`
}

// HealthPeer is a single peer gateway in the cell.
type HealthPeer struct {
	NodeID   string `json:"node_id"`
	Endpoint string `json:"endpoint"`
}

// ProvidersConfig carries per-provider settings. Phase 2 surfaces
// the full B2C / B2B / BYOC provider matrix described in
// docs/STORAGE_INFRA.md. Empty sub-configs mean "do not register
// this provider".
type ProvidersConfig struct {
	Wasabi       WasabiConfig       `json:"wasabi"`
	LocalFSDev   LocalFSDevConfig   `json:"local_fs_dev"`
	CephRGW      CephRGWConfig      `json:"ceph_rgw"`
	BackblazeB2  BackblazeB2Config  `json:"backblaze_b2"`
	CloudflareR2 CloudflareR2Config `json:"cloudflare_r2"`
	AWSS3        AWSS3Config        `json:"aws_s3"`
}

// WasabiConfig configures the Phase 1 primary storage backend.
type WasabiConfig struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// LocalFSDevConfig configures the developer-loopback adapter used by
// tests.
type LocalFSDevConfig struct {
	RootPath string `json:"root_path"`
}

// CephRGWConfig configures the Ceph RADOS Gateway backend used for
// B2B dedicated cells and sovereign placement. See
// https://docs.ceph.com/en/latest/radosgw/.
type CephRGWConfig struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Cell      string `json:"cell"`
	Country   string `json:"country"`
}

// BackblazeB2Config configures the Backblaze B2 S3-compatible
// backend.
type BackblazeB2Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// CloudflareR2Config configures the Cloudflare R2 backend.
type CloudflareR2Config struct {
	AccountID string `json:"account_id"`
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// AWSS3Config configures the AWS S3 BYOC / disaster-recovery backend.
type AWSS3Config struct {
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	Endpoint  string `json:"endpoint"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// RebalancerConfig configures the optional background rebalancer
// that sweeps manifests in a migration-in-progress state and copies
// their pieces from the old backend onto the new primary. See
// migration/background_rebalancer for the semantics.
//
// The rebalancer starts only when Enabled is true and Targets is
// non-empty. Interval controls the gap between full passes;
// BytesPerSecond caps the steady-state copy bandwidth.
//
// Enabled defaults to true when the key is omitted from the config
// file so existing deployments that only specified targets keep
// running the rebalancer after this field was introduced. Setting
// it explicitly to false disables the worker regardless of Targets.
type RebalancerConfig struct {
	Enabled        bool               `json:"enabled"`
	Interval       Duration           `json:"interval"`
	BytesPerSecond int64              `json:"bytes_per_second"`
	Targets        []RebalancerTarget `json:"targets"`
}

// UnmarshalJSON decodes a RebalancerConfig, defaulting Enabled to
// true when the "enabled" key is omitted. Explicit "enabled": false
// still disables the worker.
func (r *RebalancerConfig) UnmarshalJSON(data []byte) error {
	type alias RebalancerConfig
	tmp := alias{Enabled: true}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*r = RebalancerConfig(tmp)
	return nil
}

// RebalancerTarget names a single (tenant, bucket) pair to
// rebalance along with the source and destination backend names.
// The backend names must resolve to entries in the gateway's
// provider registry.
type RebalancerTarget struct {
	TenantID       string `json:"tenant_id"`
	Bucket         string `json:"bucket"`
	SourceBackend  string `json:"source_backend"`
	PrimaryBackend string `json:"primary_backend"`
}

// Default returns a minimal, developer-friendly configuration.
func Default() Config {
	return Config{
		Env: "development",
		Gateway: GatewayConfig{
			ListenAddr:      ":8080",
			ReadTimeout:     Duration(30 * time.Second),
			WriteTimeout:    Duration(30 * time.Second),
			MaxRequestBytes: 5 * 1024 * 1024 * 1024, // 5 GiB
			// CachePath defaults to empty so developer and test
			// environments get the in-memory cache without a
			// DiskCache-fallback warning when the host has no
			// persistent cache volume. Production gateway nodes
			// must set this via the config file (gateway.cache_path)
			// or via an environment-specific override so that NVMe /
			// block storage is used as the L0 / L1 hot-object cache.
			CachePath: "",
		},
		Providers: ProvidersConfig{
			LocalFSDev: LocalFSDevConfig{
				RootPath: "/var/lib/zk-object-fabric/local_fs_dev",
			},
		},
		Console: ConsoleConfig{
			ListenAddr:   ":8081",
			ReadTimeout:  Duration(30 * time.Second),
			WriteTimeout: Duration(30 * time.Second),
		},
	}
}

// Load reads a JSON configuration file from path and returns a fully
// populated Config, applying Default() for any omitted fields.
func Load(path string) (Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}
