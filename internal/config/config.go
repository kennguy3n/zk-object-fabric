// Package config loads runtime configuration for the gateway and the
// control-plane services. Phase 1 accepts only a minimal set of
// knobs; the file format and schema will expand as Phase 2 lands.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the top-level runtime configuration.
type Config struct {
	Env         string            `json:"env"`
	Gateway     GatewayConfig     `json:"gateway"`
	ControlPlane ControlPlaneConfig `json:"control_plane"`
	Providers   ProvidersConfig   `json:"providers"`
}

// GatewayConfig configures the S3-compatible gateway fleet on Linode.
type GatewayConfig struct {
	ListenAddr      string        `json:"listen_addr"`
	ReadTimeout     time.Duration `json:"read_timeout"`
	WriteTimeout    time.Duration `json:"write_timeout"`
	MaxRequestBytes int64         `json:"max_request_bytes"`
	CachePath       string        `json:"cache_path"`
}

// ControlPlaneConfig configures the AWS-hosted control plane surface
// the gateway talks to.
type ControlPlaneConfig struct {
	MetadataDSN string `json:"metadata_dsn"`
	AuthIssuer  string `json:"auth_issuer"`
	BillingURL  string `json:"billing_url"`
}

// ProvidersConfig carries per-provider settings. In Phase 1 only
// Wasabi and local_fs_dev are populated; other providers are
// placeholders for later phases.
type ProvidersConfig struct {
	Wasabi     WasabiConfig     `json:"wasabi"`
	LocalFSDev LocalFSDevConfig `json:"local_fs_dev"`
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

// Default returns a minimal, developer-friendly configuration.
func Default() Config {
	return Config{
		Env: "development",
		Gateway: GatewayConfig{
			ListenAddr:      ":8080",
			ReadTimeout:     30 * time.Second,
			WriteTimeout:    30 * time.Second,
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
