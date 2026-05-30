// Package config loads runtime configuration for the gateway and the
// control-plane services. Phase 1 accepts only a minimal set of
// knobs; the file format and schema will expand as Phase 2 lands.
package config

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	Dedup        DedupConfig        `json:"dedup"`
	Tracing      TracingConfig      `json:"tracing"`
	Metrics      MetricsConfig      `json:"metrics"`
	Compliance   ComplianceConfig   `json:"compliance"`
	CrossCell    CrossCellConfig    `json:"cross_cell"`
	Repair       RepairConfig       `json:"repair"`
}

// TracingConfig configures the OpenTelemetry-style request
// tracer wired into the gateway. When Enabled is false the
// gateway uses a no-op tracer; when true and Endpoint is set, a
// real exporter is constructed in cmd/gateway.
type TracingConfig struct {
	Enabled     bool   `json:"enabled"`
	Endpoint    string `json:"endpoint"`
	ServiceName string `json:"service_name"`
}

// MetricsConfig gates the Prometheus metrics endpoint. Path
// defaults to /internal/metrics.
type MetricsConfig struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
}

// ComplianceConfig configures the data-residency enforcer and the
// audit trail. When neither field is enabled the gateway behaves
// as it did before Phase 4: no residency check, no audit rows.
type ComplianceConfig struct {
	// ResidencyEnabled gates the pre-flight residency check on
	// the PUT path. Uses the per-tenant country allowlist
	// (Postgres tenant_country_allowlist or StaticAllowlist) and
	// any per-object Residency hints baked into the placement
	// policy.
	ResidencyEnabled bool `json:"residency_enabled"`

	// AuditEnabled gates writing one compliance_audit row per
	// successful PUT.
	AuditEnabled bool `json:"audit_enabled"`

	// LegalHoldEnabled gates the DELETE-path legal-hold check.
	// When true the gateway constructs a LegalHoldStore (Postgres
	// when a metadata DB is configured, in-memory otherwise) and
	// the s3compat handler refuses DELETEs covered by an active
	// hold.
	LegalHoldEnabled bool `json:"legal_hold_enabled"`

	// StaticAllowlist seeds a tenant -> country list directly
	// from config. Keys are tenant IDs; values are ISO 3166-1
	// alpha-2 country codes. Used by deployments that do not run
	// a Postgres metadata DB.
	StaticAllowlist map[string][]string `json:"static_allowlist"`
}

// CrossCellConfig configures the optional cross-cell async
// replicator. Enabled gates the worker entirely; SourceCellID /
// DestCellID identify the (src, dst) provider names in the
// gateway's provider registry. ScanInterval is the cadence at
// which the scope is re-scanned; Scope is a list of (tenant,
// bucket) tuples the worker mirrors.
type CrossCellConfig struct {
	Enabled      bool                  `json:"enabled"`
	SourceCellID string                `json:"source_cell_id"`
	DestCellID   string                `json:"dest_cell_id"`
	ScanInterval Duration              `json:"scan_interval"`
	Scope        []CrossCellScopeEntry `json:"scope"`
}

// CrossCellScopeEntry is one (tenant, bucket) tuple the
// replicator scans on each tick.
type CrossCellScopeEntry struct {
	TenantID string `json:"tenant_id"`
	Bucket   string `json:"bucket"`
}

// RepairConfig configures the optional automated repair queue.
// Enabled gates the worker entirely; CephEndpoint is the URL of
// the Ceph manager health API the queue polls; PollInterval is
// the cadence at which the source is sampled. AuthToken, when
// non-empty, is forwarded as a Bearer token on the health
// request.
type RepairConfig struct {
	Enabled      bool     `json:"enabled"`
	CephEndpoint string   `json:"ceph_endpoint"`
	AuthToken    string   `json:"auth_token"`
	PollInterval Duration `json:"poll_interval"`
}


// DedupConfig configures intra-tenant deduplication. Cross-tenant
// dedup is permanently excluded from the fabric, so DefaultScope
// is always "intra_tenant"; the field exists only to make the
// invariant visible in operator-facing config.
//
// See docs/PROPOSAL.md §3.14.
type DedupConfig struct {
	// Enabled gates dedup at the gateway level. When false the
	// content_index store is unused and every PUT writes a
	// fresh piece. Defaults to false; per-bucket / per-object
	// policy still has to opt in via PlacementPolicy.DedupPolicy.
	Enabled bool `json:"enabled"`

	// DefaultScope is always "intra_tenant". Surfaced in config
	// to make the invariant explicit.
	DefaultScope string `json:"default_scope"`

	// DefaultLevel selects the default dedup tier when a tenant
	// policy does not override it. Valid values:
	//   - "object":       gateway content_index only (all backends).
	//   - "object+block": object-level + Ceph RGW native chunk dedup
	//                     (dedicated B2B cells only).
	DefaultLevel string `json:"default_level"`

	// OrphanGCInterval gates the background orphan-row sweep that
	// removes content_index rows whose piece is no longer
	// referenced by any live manifest. Zero (or unset) disables
	// the sweep; values like "30m" or "1h" enable it.
	OrphanGCInterval Duration `json:"orphan_gc_interval"`
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

	// AllowLocalCMK downgrades the production safety check on
	// the local-file CMK wrapper from a fatal startup error to
	// a logged warning. The default (false) is fail-closed:
	// shipping env=production with cmk://local/... refuses to
	// boot, because a plaintext CMK on the gateway disk
	// trivially defeats the encryption envelope (an operator
	// with disk access can unwrap every tenant DEK).
	//
	// Set true ONLY for HSM-fuse deployments where cmk://local/
	// is the stable path to a hardware-backed key partition
	// (TPM, PKCS#11 module mapped via fuse, etc.) and the
	// underlying bytes are not actually readable as plaintext.
	// Equivalent CLI flag: --allow-local-cmk. There is no
	// substitute for getting this right: misconfigured to true
	// against a real plaintext key file, the gateway will run
	// in production with the encryption envelope effectively
	// disabled.
	AllowLocalCMK bool `json:"allow_local_cmk"`

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

	// ReadHeaderTimeout caps how long the console server waits
	// for request headers to arrive. Like the gateway's same-named
	// knob, this is the production Slowloris guard: a connected
	// client that dribbles header bytes is forcibly closed when
	// the timeout fires. The default (see Default()) is 10s,
	// matching the gateway's posture so a misconfigured ingress
	// that accidentally exposes the console API to the internet
	// is not silently exploitable. Operators who terminate TLS
	// upstream of the console server and rely on the proxy's own
	// timeouts can lower this — but it should never be 0.
	ReadHeaderTimeout Duration `json:"read_header_timeout"`

	// IdleTimeout bounds the lifetime of a keep-alive connection
	// between requests. Used to evict idle slowloris connections
	// holding sockets open with no in-flight request. Default 120s.
	IdleTimeout Duration `json:"idle_timeout"`

	// MaxHeaderBytes caps total header size for a single request.
	// Zero means "use Go's default" (1 MiB) which is far larger
	// than any well-formed console request needs and gives an
	// attacker more memory to exhaust per connection; pin a tight
	// 64 KiB ceiling by default. Matches the gateway knob.
	MaxHeaderBytes int `json:"max_header_bytes"`

	AdminToken string `json:"admin_token"`

	// TLS configures the console API's HTTPS listener. Same
	// semantics as GatewayConfig.TLS — empty CertPath / KeyPath
	// means plain HTTP, both set means HTTPS.
	TLS TLSConfig `json:"tls"`

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

	// ReadHeaderTimeout caps how long the gateway is willing to
	// wait for the request headers to finish arriving before
	// dropping the connection. A zero value here means "use
	// ReadTimeout" (Go's default), which exposes the gateway to
	// Slowloris-style attacks where a client opens many TCP
	// connections and dribbles one byte of header at a time —
	// each connection occupies a goroutine until ReadTimeout
	// expires across the entire request body. Pinning a short
	// ReadHeaderTimeout (default 10s) bounds the per-connection
	// cost of header-stalling clients regardless of how long the
	// body is.
	//
	// See tests/abuse/slowloris_test.go for the regression test
	// that pins this defence.
	ReadHeaderTimeout Duration `json:"read_header_timeout"`

	// IdleTimeout caps how long an idle keep-alive connection is
	// kept open between requests. A zero value defaults to
	// ReadTimeout, but mirroring Go's behaviour rather than
	// stating it explicitly was the bug that let Slowloris-style
	// connection-exhaustion attacks pin gateway goroutines for
	// the full read window. Default 120s — long enough to amortise
	// TCP+TLS handshake cost across burst-y S3 SDK requests,
	// short enough that a client refusing to send a follow-up
	// request loses its slot quickly.
	IdleTimeout Duration `json:"idle_timeout"`

	// MaxHeaderBytes caps the total size of request headers the
	// gateway is willing to parse. A zero value uses Go's default
	// of 1 MiB, which is wildly generous for an S3-compatible
	// API where realistic SigV4 headers are under 4 KiB. The
	// default applied here is 64 KiB — large enough for any
	// reasonable SDK and small enough that a flood of
	// oversized-header connections runs out of buffer quickly.
	MaxHeaderBytes int `json:"max_header_bytes"`

	// TLS configures the gateway's HTTPS listener. When both
	// CertPath and KeyPath are set the listener runs HTTPS;
	// otherwise it runs plain HTTP. Production deployments
	// (Env == "production") that leave TLS empty get a startup
	// WARN line — operators terminating TLS at an upstream load
	// balancer can ignore it, deployments serving clients
	// directly should heed it.
	TLS TLSConfig `json:"tls"`

	// CacheWarmingMemoryBudget caps the total bytes the gateway
	// is willing to buffer simultaneously across all cache-miss
	// warming operations on the GET path. Each fetchPiece call
	// that wants to warm the hot-object cache acquires its piece
	// size from a semaphore initialised to this budget; if the
	// budget is exhausted the request still serves the bytes but
	// skips the inline warm and emits a PromotionSignal so the
	// async worker can decide whether to warm later. This is the
	// global ceiling that prevents a burst of concurrent cache
	// misses on different pieces from OOM-killing the gateway.
	//
	// Zero (the default applied at startup, not in JSON) means
	// 512 MiB. A negative value disables the budget guard, which
	// restores the pre-PR-7 behaviour and is intended only for
	// regression-testing the old code path.
	CacheWarmingMemoryBudget int64 `json:"cache_warming_memory_budget"`
}

// TLSConfig configures an HTTPS listener. Empty CertPath / KeyPath
// disables TLS for the listener it is attached to (plain HTTP).
//
// MinVersion accepts the case-insensitive strings "1.2" or "1.3"
// and selects the minimum acceptable TLS handshake version. An
// empty value defaults to TLS 1.2 (the same default Go's
// crypto/tls applies). Operators serving clients that all support
// TLS 1.3 should set "1.3" to lock out 1.2 cipher suites entirely.
type TLSConfig struct {
	CertPath   string `json:"cert_path"`
	KeyPath    string `json:"key_path"`
	MinVersion string `json:"min_version"`
}

// Enabled reports whether the TLS config has the minimum fields
// needed to start an HTTPS listener (cert + key path both set).
//
// A partially-populated TLSConfig (exactly one of CertPath or
// KeyPath set) is NOT enabled. Operators should use Validate()
// to detect that misconfiguration explicitly; Enabled() alone
// will silently report false and cause startListener to fall
// through to plain HTTP, which is the wrong failure mode for a
// production deployment.
func (t TLSConfig) Enabled() bool {
	return t.CertPath != "" && t.KeyPath != ""
}

// Validate returns an error when the TLSConfig is partially
// populated — exactly one of CertPath or KeyPath set. Either both
// must be empty (plain HTTP) or both must be non-empty (HTTPS);
// any other shape is an operator typo that would otherwise
// silently downgrade to unencrypted traffic.
//
// Validate also exercises MinTLSVersion so an unrecognised value
// surfaces at startup rather than at the moment a client tries to
// handshake.
//
// Callers should invoke Validate before startListener picks a
// branch on Enabled(); the gateway entrypoint does this for every
// listener (gateway, console, health) at boot.
func (t TLSConfig) Validate(name string) error {
	hasCert := t.CertPath != ""
	hasKey := t.KeyPath != ""
	if hasCert != hasKey {
		set, missing := "cert_path", "key_path"
		if hasKey {
			set, missing = "key_path", "cert_path"
		}
		return fmt.Errorf("config: %s.tls.%s is set but %s is empty; both must be set for HTTPS or both must be empty for plain HTTP", name, set, missing)
	}
	if _, err := t.MinTLSVersion(); err != nil {
		return fmt.Errorf("config: %s.tls: %w", name, err)
	}
	return nil
}

// MinTLSVersion parses the configured MinVersion string and returns
// the corresponding crypto/tls constant. An empty value defaults to
// TLS 1.2 (the same default Go's net/http applies); an unrecognised
// value returns an error so misconfigurations surface at startup
// rather than as silent downgrades. TLS 1.0 / 1.1 are not
// accepted — they are no longer considered secure and crypto/tls
// has not supported them as defaults since Go 1.18.
func (t TLSConfig) MinTLSVersion() (uint16, error) {
	v := strings.TrimSpace(strings.ToLower(t.MinVersion))
	switch v {
	case "", "1.2", "tls1.2", "tls12":
		return tls.VersionTLS12, nil
	case "1.3", "tls1.3", "tls13":
		return tls.VersionTLS13, nil
	default:
		return 0, fmt.Errorf("tls.min_version %q: must be \"1.2\" or \"1.3\"", t.MinVersion)
	}
}

// BuildGoTLSConfig returns the *tls.Config the gateway hands to
// http.Server.TLSConfig before calling ListenAndServeTLS. It
// honours MinVersion (defaulting to TLS 1.2).
//
// The function does not load the cert/key itself — that is
// http.Server.ListenAndServeTLS's job — it only builds the
// crypto/tls policy.
//
// Note: PreferServerCipherSuites is intentionally not set. The
// field has been a no-op in crypto/tls since Go 1.18; the
// implementation always uses a fixed, security-driven cipher
// preference order regardless of the flag, and staticcheck flags
// any explicit assignment as SA1019.
func (t TLSConfig) BuildGoTLSConfig() (*tls.Config, error) {
	minVer, err := t.MinTLSVersion()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: minVer,
	}, nil
}

// ControlPlaneConfig configures the AWS-hosted control plane surface
// the gateway talks to.
//
// The connection-pool fields tune the single RDS-backed *sql.DB
// the gateway opens for the metadata DSN. All five Postgres-backed
// stores (manifest, tenant, auth, placement, dedicated cell) share
// that one pool, so MaxOpenConns is the gateway-process-wide cap
// on metadata connections — not a per-store multiplier. Production
// deploys typically set MaxOpenConns to 2× the gateway's CPU count
// and ConnMaxLifetime to a value comfortably under RDS Proxy's
// idle-connection timeout (10 minutes by default).
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

	// TLS configures the health endpoint's HTTPS listener. Same
	// semantics as GatewayConfig.TLS — empty CertPath / KeyPath
	// means plain HTTP, both set means HTTPS. Operators terminating
	// TLS at a load balancer typically leave this empty even in
	// production.
	TLS TLSConfig `json:"tls"`
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
// configs and register the provider under the bare name "wasabi".
// Phase 3 production deploys set Regions instead, registering one
// Wasabi provider per region under "wasabi-<region>" (or the
// region's explicit `name`). The two paths are independent — both
// can be set at once and they all register side-by-side.
// pickDefaultBackend treats any "wasabi-*" entry as a substitute
// for "wasabi" in its preference order, so a pure multi-region
// config still boots with a Wasabi default.
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
	// NodeID identifies this gateway instance for the
	// distributed coordination JobStore. Two gateways with
	// the same NodeID share claim ownership, which means
	// either's heartbeat will keep the other's claim alive —
	// usually a misconfiguration. Default: the OS hostname,
	// resolved by cmd/gateway when this field is left empty.
	// Setting it explicitly is the recommended path for
	// containerised deployments where the hostname is a
	// random hash that does not correlate with the pod
	// identity an operator wants to see in the queue.
	NodeID string `json:"node_id"`
	// ClaimTTL is the TTL each AcquireJob writes onto the
	// claim. The worker heartbeats at ClaimTTL/2 to stay
	// well inside the expiry window. Zero defaults to 30s,
	// which keeps a crashed node's jobs recoverable within a
	// minute without flooding the DB with heartbeat traffic
	// on busy fleets.
	ClaimTTL Duration `json:"claim_ttl"`
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
			ListenAddr:               ":8080",
			ReadTimeout:              Duration(30 * time.Second),
			WriteTimeout:             Duration(30 * time.Second),
			ReadHeaderTimeout:        Duration(10 * time.Second),
			IdleTimeout:              Duration(120 * time.Second),
			MaxHeaderBytes:           64 * 1024,
			MaxRequestBytes:          5 * 1024 * 1024 * 1024, // 5 GiB
			CacheWarmingMemoryBudget: 512 * 1024 * 1024,      // 512 MiB
			// CachePath defaults to empty so developer and test
			// environments get the in-memory cache without a
			// DiskCache-fallback warning when the host has no
			// persistent cache volume. Production gateway nodes
			// must set this via the config file (gateway.cache_path)
			// or via an environment-specific override so that NVMe /
			// block storage is used as the L0 / L1 hot-object cache.
			CachePath: "",
		},
		// ControlPlane defaults intentionally pin the metadata
		// connection-pool ceilings to safe values rather than
		// inheriting Go's stdlib defaults (unlimited MaxOpenConns,
		// MaxIdleConns=2, no connection-lifetime cap). The stdlib
		// defaults will quickly saturate RDS or trip RDS Proxy's
		// idle-connection timeout in production. These numbers
		// match the comment block on ControlPlaneConfig: 32 open
		// connections is a comfortable per-gateway-node ceiling
		// against a t3.medium / db.t3.medium RDS class, and a
		// 4-minute lifetime stays well under RDS Proxy's
		// 10-minute idle timeout so the pool naturally rotates
		// connections before the proxy retires them. Operators
		// override any of these via config.control_plane.* when
		// their RDS class can support a larger pool.
		ControlPlane: ControlPlaneConfig{
			MaxOpenConns:    32,
			MaxIdleConns:    8,
			ConnMaxLifetime: Duration(4 * time.Minute),
			ConnMaxIdleTime: Duration(2 * time.Minute),
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
			ListenAddr:        "",
			ReadTimeout:       Duration(30 * time.Second),
			WriteTimeout:      Duration(30 * time.Second),
			ReadHeaderTimeout: Duration(10 * time.Second),
			IdleTimeout:       Duration(120 * time.Second),
			MaxHeaderBytes:    64 * 1024,
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
