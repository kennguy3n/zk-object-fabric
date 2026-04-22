// Command gateway is the entry point for the S3-compatible ZK Gateway
// that runs on the Linode data plane. See docs/PROPOSAL.md §3.1.
//
// Phase 2 wires the HTTP surface end-to-end: a Postgres (or memory)
// ManifestStore, the provider registry, the placement engine, the
// HMAC authenticator, the hot object cache, and a logging billing
// sink. Subsequent phases swap the billing sink for ClickHouse and
// the tenant store for a Postgres-backed directory.
package main

import (
	"database/sql"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	pgstore "github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/postgres"
	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/aws_s3"
	"github.com/kennguy3n/zk-object-fabric/providers/backblaze_b2"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/cloudflare_r2"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
)

func main() {
	cfgPath := flag.String("config", "", "path to JSON config file (optional)")
	tenantsPath := flag.String("tenants", "", "path to JSON tenant bindings (optional)")
	flag.Parse()

	cfg := config.Default()
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("gateway: load config: %v", err)
		}
		cfg = loaded
	}

	store := buildManifestStore(cfg)
	registry := buildProviderRegistry(cfg)
	defaultBackend := pickDefaultBackend(registry)
	if defaultBackend == "" {
		log.Fatalf("gateway: no storage providers registered; configure at least one in config.providers")
	}

	placement := placement_policy.NewEngine(defaultBackend, registry, nil)
	tenantStore := buildTenantStore(*tenantsPath)
	authenticator := auth.NewHMACAuthenticator(tenantStore)
	billingSink := &billing.LoggerSink{Logger: log.New(os.Stdout, "", log.LstdFlags)}

	cache, err := hot_object_cache.NewMemoryCache(hot_object_cache.DefaultEvictionPolicy(1 << 30))
	if err != nil {
		log.Fatalf("gateway: build hot object cache: %v", err)
	}
	signalBus := hot_object_cache.NewSignalBus(1024)

	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests:      store,
		Providers:      registry,
		Placement:      placement,
		Auth:           authenticator,
		Billing:        billingSink,
		Cache:          cache,
		CachePublisher: signalBus,
		NodeID:         cfg.Env,
	}).Register(mux)

	handler := http.Handler(mux)
	if tenantStore.Size() > 0 {
		rl := auth.NewRateLimiter(
			auth.TenantBudgetsLookup(tenantStore),
			auth.TenantResolverFromAuth(authenticator),
		)
		handler = rl.Middleware(mux)
	}

	srv := &http.Server{
		Addr:         cfg.Gateway.ListenAddr,
		Handler:      handler,
		ReadTimeout:  cfg.Gateway.ReadTimeout.ToDuration(),
		WriteTimeout: cfg.Gateway.WriteTimeout.ToDuration(),
	}

	log.Printf("gateway: listening on %s (env=%s default_backend=%s)", cfg.Gateway.ListenAddr, cfg.Env, defaultBackend)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("gateway: listen: %v", err)
	}
}

func buildManifestStore(cfg config.Config) manifest_store.ManifestStore {
	if cfg.ControlPlane.MetadataDSN == "" {
		log.Printf("gateway: no control_plane.metadata_dsn; using in-memory manifest store (dev only)")
		return memory.New()
	}
	db, err := sql.Open("postgres", cfg.ControlPlane.MetadataDSN)
	if err != nil {
		log.Fatalf("gateway: open postgres: %v", err)
	}
	if err := db.Ping(); err != nil {
		log.Fatalf("gateway: ping postgres: %v", err)
	}
	store, err := pgstore.New(pgstore.Config{DB: db})
	if err != nil {
		log.Fatalf("gateway: build postgres manifest store: %v", err)
	}
	return store
}

func buildProviderRegistry(cfg config.Config) map[string]providers.StorageProvider {
	registry := map[string]providers.StorageProvider{}

	if cfg.Providers.LocalFSDev.RootPath != "" {
		p, err := local_fs_dev.New(cfg.Providers.LocalFSDev.RootPath)
		if err != nil {
			log.Fatalf("gateway: build local_fs_dev: %v", err)
		}
		registry["local_fs_dev"] = p
	}
	if cfg.Providers.Wasabi.Endpoint != "" {
		w, err := wasabi.New(wasabi.Config{
			Endpoint:  cfg.Providers.Wasabi.Endpoint,
			Region:    cfg.Providers.Wasabi.Region,
			Bucket:    cfg.Providers.Wasabi.Bucket,
			AccessKey: cfg.Providers.Wasabi.AccessKey,
			SecretKey: cfg.Providers.Wasabi.SecretKey,
		})
		if err != nil {
			log.Fatalf("gateway: build wasabi: %v", err)
		}
		registry["wasabi"] = w
	}
	if cfg.Providers.CephRGW.Endpoint != "" {
		c, err := ceph_rgw.New(ceph_rgw.Config{
			Endpoint:  cfg.Providers.CephRGW.Endpoint,
			Region:    cfg.Providers.CephRGW.Region,
			Bucket:    cfg.Providers.CephRGW.Bucket,
			AccessKey: cfg.Providers.CephRGW.AccessKey,
			SecretKey: cfg.Providers.CephRGW.SecretKey,
			Cell:      cfg.Providers.CephRGW.Cell,
			Country:   cfg.Providers.CephRGW.Country,
		})
		if err != nil {
			log.Fatalf("gateway: build ceph_rgw: %v", err)
		}
		registry["ceph_rgw"] = c
	}
	if cfg.Providers.BackblazeB2.Bucket != "" {
		b, err := backblaze_b2.New(backblaze_b2.Config{
			Endpoint:  cfg.Providers.BackblazeB2.Endpoint,
			Region:    cfg.Providers.BackblazeB2.Region,
			Bucket:    cfg.Providers.BackblazeB2.Bucket,
			AccessKey: cfg.Providers.BackblazeB2.AccessKey,
			SecretKey: cfg.Providers.BackblazeB2.SecretKey,
		})
		if err != nil {
			log.Fatalf("gateway: build backblaze_b2: %v", err)
		}
		registry["backblaze_b2"] = b
	}
	if cfg.Providers.CloudflareR2.Bucket != "" {
		r, err := cloudflare_r2.New(cloudflare_r2.Config{
			AccountID: cfg.Providers.CloudflareR2.AccountID,
			Endpoint:  cfg.Providers.CloudflareR2.Endpoint,
			Bucket:    cfg.Providers.CloudflareR2.Bucket,
			AccessKey: cfg.Providers.CloudflareR2.AccessKey,
			SecretKey: cfg.Providers.CloudflareR2.SecretKey,
		})
		if err != nil {
			log.Fatalf("gateway: build cloudflare_r2: %v", err)
		}
		registry["cloudflare_r2"] = r
	}
	if cfg.Providers.AWSS3.Bucket != "" {
		a, err := aws_s3.New(aws_s3.Config{
			Region:    cfg.Providers.AWSS3.Region,
			Bucket:    cfg.Providers.AWSS3.Bucket,
			Endpoint:  cfg.Providers.AWSS3.Endpoint,
			AccessKey: cfg.Providers.AWSS3.AccessKey,
			SecretKey: cfg.Providers.AWSS3.SecretKey,
		})
		if err != nil {
			log.Fatalf("gateway: build aws_s3: %v", err)
		}
		registry["aws_s3"] = a
	}
	return registry
}

// pickDefaultBackend returns the first backend name in a stable
// preference order so the gateway boots with a usable placement
// default even without explicit tenant policies.
func pickDefaultBackend(registry map[string]providers.StorageProvider) string {
	for _, name := range []string{"wasabi", "ceph_rgw", "backblaze_b2", "cloudflare_r2", "aws_s3", "local_fs_dev"} {
		if _, ok := registry[name]; ok {
			return name
		}
	}
	return ""
}

func buildTenantStore(path string) *auth.MemoryTenantStore {
	store := auth.NewMemoryTenantStore()
	if path == "" {
		return store
	}
	if err := store.LoadBindingsFromJSON(path); err != nil {
		log.Fatalf("gateway: load tenants: %v", err)
	}
	return store
}
