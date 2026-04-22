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
	"context"
	"database/sql"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/internal/health"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	pgstore "github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/postgres"
	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/migration/background_rebalancer"
	"github.com/kennguy3n/zk-object-fabric/migration/lazy_read_repair"
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
	billingSink := buildBillingSink(cfg)

	cache, err := buildHotObjectCache(cfg)
	if err != nil {
		log.Fatalf("gateway: build hot object cache: %v", err)
	}
	signalBus := hot_object_cache.NewSignalBus(1024)

	workerCtx, cancelWorker := context.WithCancel(context.Background())
	defer cancelWorker()
	worker := &hot_object_cache.PromotionWorker{
		Cache:    cache,
		Policies: hot_object_cache.DefaultPromotionPolicies(),
		Fetcher:  hot_object_cache.StaticFetcher{Provider: registry[defaultBackend]},
		Logger:   log.New(os.Stdout, "promotion ", log.LstdFlags),
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(workerCtx, signalBus.Channel())
	}()

	readRepair := lazy_read_repair.New(registry, store)
	readRepair.Logger = log.New(os.Stdout, "read_repair ", log.LstdFlags)

	rebalancerDone := startRebalancer(workerCtx, cfg.Rebalancer, store, registry)

	healthMon := startHealthMonitor(workerCtx, cfg.Health, cache)

	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests:      store,
		Providers:      registry,
		Placement:      placement,
		Auth:           authenticator,
		Billing:        billingSink,
		Cache:          cache,
		CachePublisher: signalBus,
		ReadRepair:     readRepair,
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

	shutdownCh := make(chan os.Signal, 1)
	signal.Notify(shutdownCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdownCh
		log.Printf("gateway: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("gateway: http shutdown: %v", err)
		}
		if healthMon != nil {
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := healthMon.Drain(drainCtx); err != nil {
				log.Printf("gateway: health drain: %v", err)
			}
			drainCancel()
		}
		if closer, ok := billingSink.(interface {
			Close(context.Context) error
		}); ok {
			billingCtx, billingCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := closer.Close(billingCtx); err != nil {
				log.Printf("gateway: close billing sink: %v", err)
			}
			billingCancel()
		}
		signalBus.Close()
		cancelWorker()
	}()

	log.Printf("gateway: listening on %s (env=%s default_backend=%s)", cfg.Gateway.ListenAddr, cfg.Env, defaultBackend)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway: listen: %v", err)
	}
	<-workerDone
	if rebalancerDone != nil {
		<-rebalancerDone
	}
}

// startRebalancer spins up the background_rebalancer on a ticker
// when the gateway config enables it and names one or more
// migration targets. It returns a channel that closes when the
// rebalancer goroutine has fully drained, or nil when no
// rebalancer was started. The rebalancer shares ctx with the
// promotion worker so a SIGTERM-triggered cancelWorker() also
// stops the rebalancer.
func startRebalancer(
	ctx context.Context,
	rb config.RebalancerConfig,
	store manifest_store.ManifestStore,
	registry map[string]providers.StorageProvider,
) <-chan struct{} {
	if !rb.Enabled || len(rb.Targets) == 0 {
		return nil
	}
	targets := make([]background_rebalancer.TenantTarget, 0, len(rb.Targets))
	for _, t := range rb.Targets {
		targets = append(targets, background_rebalancer.TenantTarget{
			TenantID:       t.TenantID,
			Bucket:         t.Bucket,
			SourceBackend:  t.SourceBackend,
			PrimaryBackend: t.PrimaryBackend,
		})
	}
	interval := rb.Interval.ToDuration()
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	reb := background_rebalancer.New(background_rebalancer.Config{
		Manifests:      store,
		Providers:      registry,
		Targets:        targets,
		BytesPerSecond: rb.BytesPerSecond,
		Logger:         log.New(os.Stdout, "rebalancer ", log.LstdFlags),
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			stats, err := reb.Run(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("gateway: rebalancer pass: %v", err)
			}
			if stats.PiecesCopied > 0 || stats.PhasesAdvanced > 0 {
				log.Printf("gateway: rebalancer scanned=%d copied=%d bytes=%d advanced=%d errors=%d",
					stats.ManifestsScanned, stats.PiecesCopied, stats.BytesCopied, stats.PhasesAdvanced, stats.Errors)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
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

// buildHotObjectCache returns a DiskCache when cfg.Gateway.CachePath
// is set and a writable directory, falling back to an in-memory
// cache for developer/test flows. The cache capacity is 1 GiB so
// small dev machines don't fill the disk; operators size it via
// the eviction policy in Phase 4's config refactor.
func buildHotObjectCache(cfg config.Config) (hot_object_cache.HotObjectCache, error) {
	policy := hot_object_cache.DefaultEvictionPolicy(1 << 30)
	if cfg.Gateway.CachePath == "" {
		return hot_object_cache.NewMemoryCache(policy)
	}
	return hot_object_cache.NewDiskCache(hot_object_cache.DiskCacheConfig{
		RootPath: cfg.Gateway.CachePath,
		Policy:   policy,
	})
}

// buildBillingSink returns the ClickHouseSink when billing is
// configured, otherwise the development LoggerSink. The returned
// value satisfies api/s3compat.BillingSink.
func buildBillingSink(cfg config.Config) interface {
	Emit(event billing.UsageEvent)
} {
	if cfg.Billing.ClickHouseURL == "" {
		return &billing.LoggerSink{Logger: log.New(os.Stdout, "", log.LstdFlags)}
	}
	sink, err := billing.NewClickHouseSink(billing.ClickHouseConfig{
		Endpoint:      cfg.Billing.ClickHouseURL,
		Database:      cfg.Billing.ClickHouseDatabase,
		Table:         cfg.Billing.ClickHouseTable,
		Username:      cfg.Billing.ClickHouseUsername,
		Password:      cfg.Billing.ClickHousePassword,
		BatchSize:     cfg.Billing.BatchSize,
		FlushInterval: cfg.Billing.FlushInterval.ToDuration(),
		Logger:        log.New(os.Stdout, "billing ", log.LstdFlags),
	})
	if err != nil {
		log.Fatalf("gateway: build clickhouse billing sink: %v", err)
	}
	return sink
}

// startHealthMonitor starts the gateway fleet node health monitor
// and, when a listen address is configured, the internal HTTP
// endpoints it exposes. The monitor shares ctx with the other
// background workers so SIGTERM drains all of them together.
func startHealthMonitor(ctx context.Context, hc config.HealthConfig, cache hot_object_cache.HotObjectCache) *health.Monitor {
	nodeID := hc.NodeID
	if nodeID == "" {
		if name, err := os.Hostname(); err == nil {
			nodeID = name
		} else {
			nodeID = "gateway"
		}
	}
	peers := make([]health.Peer, 0, len(hc.Peers))
	for _, p := range hc.Peers {
		peers = append(peers, health.Peer{NodeID: p.NodeID, Endpoint: p.Endpoint})
	}
	mon, err := health.New(health.Config{
		NodeID:          nodeID,
		CellID:          hc.CellID,
		Peers:           peers,
		QuorumThreshold: hc.QuorumThreshold,
		PollInterval:    hc.PollInterval.ToDuration(),
		PollTimeout:     hc.PollTimeout.ToDuration(),
		DrainTimeout:    hc.DrainTimeout.ToDuration(),
		Cache:           cache,
		Logger:          log.New(os.Stdout, "health ", log.LstdFlags),
	})
	if err != nil {
		log.Fatalf("gateway: build health monitor: %v", err)
	}
	go func() { _ = mon.Run(ctx) }()
	if hc.ListenAddr != "" {
		srv := &http.Server{Addr: hc.ListenAddr, Handler: mon.ServeMux("")}
		go func() {
			log.Printf("gateway: health endpoints on %s", hc.ListenAddr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("gateway: health listener: %v", err)
			}
		}()
		go func() {
			<-ctx.Done()
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdownCtx)
		}()
	}
	return mon
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
