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
	Encryption   EncryptionConfig   `json:"encryption"`
	Abuse        AbuseConfig        `json:"abuse"`
}

// AbuseConfig tunes the per-region runtime knobs of the abuse
// guard and rate limiter (internal/auth/abuse.go,
// internal/auth/rate_limit.go) and selects an optional production
// alert webhook the gateway fans out anomaly / budget events to in
// addition to the billing sink.
//
// Operators run multiple gateway fleets per region with different
// traffic patterns, so the EWMA baseline, anomaly multiplier, and
// cooldown window are surfaced in the config rather than the
// per-tenant tenant.Abuse record. Leave fields zero to inherit the
// in-package defaults (see NewAbuseGuard / NewRateLimiter).
type AbuseConfig struct {
	// AnomalyMultiplier is the ratio of current egress / request
	// rate to baseline that fires an alert. Zero inherits the
	// 2x-of-baseline default.
	AnomalyMultiplier float64 `json:"anomaly_multiplier"`

	// AnomalyWindow is the sliding window the rate is measured
	// over. Zero inherits the in-package default (1 minute).
	AnomalyWindow Duration `json:"anomaly_window"`

	// AnomalyCooldown debounces repeated alerts and bounds the
	// 429 throttle window when ThrottleOnAnomaly is true. Zero
	// inherits AnomalyWindow.
	AnomalyCooldown Duration `json:"anomaly_cooldown"`

	// ThrottleOnAnomaly returns HTTP 429 for follow-up requests
	// inside the cooldown window when an anomaly fires. Defaults
	// to alert-only.
	ThrottleOnAnomaly bool `json:"throttle_on_anomaly"`

	// BaselineAlpha is the EWMA weight applied to each completed
	// window. Must be in (0, 1]; zero inherits 0.3.
	BaselineAlpha float64 `json:"baseline_alpha"`

	// AlertWebhookURL, when set, receives a JSON-encoded
	// billing.UsageEvent for every abuse alert in addition to
	// the billing sink. PagerDuty / Slack / generic webhook.
	AlertWebhookURL string `json:"alert_webhook_url"`
}

// EncryptionConfig configures the gateway's DEK-wrapping material
// for "managed" and "public_distribution" tenant policies, plus
// the optional manifest body encryption key.
//
// Phase 2 wires the wrapper to a 32-byte local key file
// (CMKPath). Phase 3 replaces this with a KMS ARN or Vault
// transit path; both are consumed through the same
// client_sdk.Wrapper interface.
//
// ManifestBodyKeyPath is the separate gateway-held key the
// Postgres manifest store uses to seal manifest JSON at rest
// (see metadata/manifest_store/postgres BodyEncryptor). A tenant
// or operator with only Postgres access cannot read manifests
// when this is set. Leave empty to keep the Phase 2 JSONB
// layout.
type EncryptionConfig struct {
	CMKPath             string `json:"cmk_path"`
	CMKURI              string `json:"cmk_uri"`
	ManifestBodyKeyPath string `json:"manifest_body_key_path"`

	// KMSRegion configures the AWS region the KMS client connects
	// to when CMKURI selects an AWS KMS wrapper. When empty the
	// client honours the AWS_REGION environment variable.
	KMSRegion string `json:"kms_region"`

	// VaultAddr is the Vault server URL used when CMKURI selects
	// the Vault transit wrapper (e.g. "https://vault.internal:8200").
	// When empty the wrapper falls back to the VAULT_ADDR
	// environment variable.
	VaultAddr string `json:"vault_addr"`

	// VaultToken authenticates to Vault. When empty the wrapper
	// falls back to the VAULT_TOKEN environment variable.
	VaultToken string `json:"vault_token"`

	// VaultTransitMount overrides the default Transit mount path
	// ("transit"). Operators that mount Transit elsewhere supply
	// the path here.
	VaultTransitMount string `json:"vault_transit_mount"`
}

// ConsoleConfig configures the tenant-console HTTP surface (api/console).
// The console runs on its own listener so a saturated S3 data plane
// cannot starve the management controls. Leave ListenAddr empty to
// disable the console API entirely.
//
// AdminToken is a shared secret the gateway checks on every
// non-auth console request via `Authorization: Bearer <token>`.
// When empty the console API is served without authentication,
// which is only safe in development or when an upstream reverse
// proxy enforces authentication out-of-band. Production deploys
// should always set this alongside ListenAddr.
type ConsoleConfig struct {
	ListenAddr   string   `json:"listen_addr"`
	ReadTimeout  Duration `json:"read_timeout"`
	WriteTimeout Duration `json:"write_timeout"`
	AdminToken   string   `json:"admin_token"`

	// CaptchaProvider names the CAPTCHA backend wired onto the B2C
	// signup flow. Supported values: "hcaptcha" (default when empty
	// and CaptchaSecret is set), "recaptcha" (reserved; not wired
	// yet), or "" (disabled). When disabled the signup handler
	// accepts requests without a CAPTCHA token; operators running
	// public signup MUST configure this.
	CaptchaProvider string `json:"captcha_provider"`

	// CaptchaSecret is the site secret the configured provider
	// validates tokens against. For hCaptcha this is the secret
	// from the hCaptcha dashboard (NEVER the site key). When
	// empty the signup handler falls back to the environment
	// variable HCAPTCHA_SECRET for backwards compatibility.
	CaptchaSecret string `json:"captcha_secret"`
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
//
// The connection-pool fields tune the RDS-backed *sql.DB sessions
// used by every Postgres-backed store (manifest, tenant, auth,
// placement, dedicated cell). Production deploys typically set
// MaxOpenConns to 2× the gateway's CPU count and ConnMaxLifetime to
// a value comfortably under RDS Proxy's idle-connection timeout
// (10 minutes by default).
type ControlPlaneConfig struct {
	MetadataDSN string `json:"metadata_dsn"`
	AuthIssuer  string `json:"auth_issuer"`
	BillingURL  string `json:"billing_url"`

	// MaxOpenConns caps concurrent open connections to the metadata
	// database. Zero means use Go's default (unlimited), which is
	// fine for dev but can saturate RDS in production.
	MaxOpenConns int `json:"max_open_conns"`

	// MaxIdleConns caps idle connections kept in the pool. Zero
	// means use Go's default (2). RDS Proxy deployments commonly
	// set this to MaxOpenConns to keep the pool warm.
	MaxIdleConns int `json:"max_idle_conns"`

	// ConnMaxLifetime caps how long a single connection may live
	// before the pool retires it. Zero means connections live
	// forever; production deploys should set this below the
	// upstream proxy's idle timeout.
	ConnMaxLifetime Duration `json:"conn_max_lifetime"`

	// ConnMaxIdleTime caps how long an idle connection may sit in
	// the pool before retirement. Zero means no limit.
	ConnMaxIdleTime Duration `json:"conn_max_idle_time"`
}

// BillingConfig configures the metering sink. When ClickHouseURL is
// empty the gateway falls back to the development LoggerSink.
//
// BillingConfig.Provider is the optional outbound integration to
// an external invoicing / payment system (Stripe, Chargebee, …).
// Phase 3 only registers the "noop" provider; future plug-ins
// drop in behind the billing.BillingProvider interface without
// any other code in the codebase needing to learn about a
// specific vendor.
type BillingConfig struct {
	ClickHouseURL      string   `json:"clickhouse_url"`
	ClickHouseDatabase string   `json:"clickhouse_database"`
	ClickHouseTable    string   `json:"clickhouse_table"`
	ClickHouseUsername string   `json:"clickhouse_username"`
	ClickHousePassword string   `json:"clickhouse_password"`
	BatchSize          int      `json:"batch_size"`
	FlushInterval      Duration `json:"flush_interval"`

	// Provider selects the BillingProvider integration. Empty
	// (or "noop") wires the no-op default that logs every call
	// without making outbound requests. Future plug-ins (e.g.
	// "stripe") register themselves under this key.
	Provider string `json:"provider"`

	// ProviderConfig is a free-form key/value map handed to the
	// selected provider's factory. Vendor-specific keys live
	// here so adding a new provider does not require widening
	// BillingConfig with vendor-specific fields.
	ProviderConfig map[string]string `json:"provider_config"`
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
	Storj        StorjConfig        `json:"storj"`
}

// WasabiConfig configures the Phase 1 primary storage backend.
//
// The single-region fields (Endpoint, Region, Bucket, AccessKey,
// SecretKey) are kept for backward compatibility with Phase 1 / 2
// configs. Phase 3 production deploys set Regions instead, so the
// gateway registers one Wasabi provider per region under the
// stable names "wasabi-<region>" (or the region's explicit
// `name`). When Regions is non-empty the legacy single-region
// fields are ignored unless the legacy fields name a region not
// already covered by Regions, in which case they are appended
// under the name "wasabi" for compatibility with the existing
// preference order in pickDefaultBackend.
type WasabiConfig struct {
	// Single-region (legacy / dev) configuration.
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`

	// Regions enumerates per-region Wasabi providers. Each entry
	// becomes its own StorageProvider keyed in the registry under
	// `Name` (defaulting to "wasabi-<Region>") so PlacementPolicy
	// can target a specific region by name.
	Regions []WasabiRegionConfig `json:"regions"`
}

// WasabiRegionConfig configures a single Wasabi region.
type WasabiRegionConfig struct {
	// Name is the registry key under which this region is
	// registered. Defaults to "wasabi-<Region>" when empty.
	Name string `json:"name"`

	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	Bucket    string `json:"bucket"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// ResolvedName returns the registry key for this region,
// computing the "wasabi-<Region>" default when Name is empty.
func (w WasabiRegionConfig) ResolvedName() string {
	if w.Name != "" {
		return w.Name
	}
	if w.Region != "" {
		return "wasabi-" + w.Region
	}
	return "wasabi"
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

// StorjConfig configures the Storj decentralized-storage BYOC
// backend. The gateway parses AccessGrant into an *uplink.Access
// and opens an *uplink.Project at startup; Bucket is the target
// bucket within the access grant's project. SatelliteAddress is
// optional: when empty the satellite embedded in the access grant
// is used, which is the supported path for production deploys.
type StorjConfig struct {
	AccessGrant      string `json:"access_grant"`
	Bucket           string `json:"bucket"`
	SatelliteAddress string `json:"satellite_address"`
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
		// Console API is opt-in: ListenAddr defaults to "" so that
		// a deployment which upgrades without an explicit console
		// block in its config does not start serving the
		// unauthenticated console API (see api/console/). Operators
		// who want the console explicitly set config.console.listen_addr
		// (":8081" is the conventional port) alongside an admin
		// authenticator at the reverse-proxy layer.
		Console: ConsoleConfig{
			ListenAddr:   "",
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
