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
	Migration    MigrationConfig    `json:"migration"`
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

// ProvidersConfig carries per-provider settings. Phase 2 surfaces
// the full B2C / B2B / BYOC provider matrix described in
// docs/STORAGE_INFRA.md. Empty sub-configs mean "do not register
// this provider".
type ProvidersConfig struct {
	Wasabi     WasabiConfig     `json:"wasabi"`
	LocalFSDev LocalFSDevConfig `json:"local_fs_dev"`
	CephRGW    CephRGWConfig    `json:"ceph_rgw"`
	BackblazeB2 BackblazeB2Config `json:"backblaze_b2"`
	CloudflareR2 CloudflareR2Config `json:"cloudflare_r2"`
	AWSS3      AWSS3Config      `json:"aws_s3"`
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

// MigrationConfig configures the optional background rebalancer that
// sweeps manifests in a migration-in-progress state and copies their
// pieces from the old backend onto the new primary. See
// migration/background_rebalancer for the semantics.
//
// When Targets is empty the rebalancer does not start. Interval
// controls the gap between full passes; BytesPerSecond caps the
// steady-state copy bandwidth.
type MigrationConfig struct {
	Targets        []MigrationTarget `json:"targets"`
	BytesPerSecond int64             `json:"bytes_per_second"`
	Interval       Duration          `json:"interval"`
}

// MigrationTarget names a single (tenant, bucket) pair to rebalance
// along with the source and destination backend names. The backend
// names must resolve to entries in the gateway's provider registry.
type MigrationTarget struct {
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
			CachePath:       "/var/lib/zk-object-fabric/cache",
		},
		Providers: ProvidersConfig{
			LocalFSDev: LocalFSDevConfig{
				RootPath: "/var/lib/zk-object-fabric/local_fs_dev",
			},
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
