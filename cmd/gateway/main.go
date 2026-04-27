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
	"crypto/subtle"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kennguy3n/zk-object-fabric/api/console"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/cellops"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/internal/health"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	cipostgres "github.com/kennguy3n/zk-object-fabric/metadata/content_index/postgres"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	pgstore "github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/postgres"
	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
	"github.com/kennguy3n/zk-object-fabric/migration/background_rebalancer"
	"github.com/kennguy3n/zk-object-fabric/migration/lazy_read_repair"
	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/aws_s3"
	"github.com/kennguy3n/zk-object-fabric/providers/backblaze_b2"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/cloudflare_r2"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	"github.com/kennguy3n/zk-object-fabric/providers/storj"
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

	// Open exactly one *sql.DB for the metadata DSN and share it
	// across every Postgres-backed store. This keeps
	// ControlPlaneConfig.MaxOpenConns as a single
	// gateway-process-wide cap on metadata connections instead of
	// a per-store multiplier.
	metadataDB, err := openMetadataDB(cfg)
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	if metadataDB != nil {
		defer func() { _ = metadataDB.Close() }()
	}

	store := buildManifestStore(cfg, metadataDB)
	contentIndex := buildContentIndex(cfg, metadataDB)
	registry := buildProviderRegistry(context.Background(), cfg)
	defaultBackend := pickDefaultBackend(registry)
	if defaultBackend == "" {
		log.Fatalf("gateway: no storage providers registered; configure at least one in config.providers")
	}

	placement := placement_policy.NewEngine(defaultBackend, registry, nil)
	tenantStore := buildTenantStore(metadataDB, *tenantsPath)
	authenticator := auth.NewHMACAuthenticator(tenantStore)
	billingSink := buildBillingSink(cfg)
	// billingProvider is the optional outbound integration to an
	// invoicing / payment system. The default is the no-op
	// provider so deployments without a real plug-in still get a
	// working gateway and a full audit trail.
	billingProvider := buildBillingProvider(cfg)
	// authStore is the B2C signup / login backing store. Created
	// here (rather than inside startConsoleAPI) so the S3 handler's
	// VerifiedCheck hook and the console's auth routes share the
	// same view of (tenant → verified) state. The Postgres-backed
	// store is selected when a metadata DSN is configured;
	// otherwise the dev MemoryAuthStore is used.
	authStore := buildAuthStore(metadataDB)
	// authHooks is built once and shared between the console API
	// and the S3 handler's email-verification gate. When
	// SendVerificationEmail is nil (no SES / transactional email
	// configured), no one can ever complete verification, so the
	// S3 gate must stay OFF — otherwise every B2C signup tenant
	// would be permanently blocked from uploading. Scaffold /
	// HMAC-only deployments therefore run without the gate.
	authHooks := buildAuthHooks(cfg)
	var verifiedCheck func(tenantID string) (verified, tracked bool)
	if authHooks.SendVerificationEmail != nil {
		verifiedCheck = authStore.IsVerified
	} else {
		log.Printf("gateway: email verification hook not configured; S3 VerifiedCheck gate disabled")
	}

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
	orphanGCDone := startOrphanGC(workerCtx, cfg.Dedup, contentIndex, store, registry)

	healthMon := startHealthMonitor(workerCtx, cfg.Health, cache)

	multipartStore := multipart.NewMemoryStore()
	erasureRegistry := erasure_coding.DefaultRegistry()

	gatewayEnc := buildGatewayEncryption(cfg.Encryption)

	mux := http.NewServeMux()
	s3compat.New(s3compat.Config{
		Manifests:      store,
		Providers:      registry,
		Placement:      placement,
		Auth:           authenticator,
		VerifiedCheck:  verifiedCheck,
		Billing:        billingSink,
		Multipart:      multipartStore,
		ErasureCoding:  erasureRegistry,
		Cache:          cache,
		CachePublisher: signalBus,
		ReadRepair:     readRepair,
		Encryption:     gatewayEnc,
		ContentIndex:   contentIndex,
		NodeID:         cfg.Env,
	}).Register(mux)

	handler := http.Handler(mux)
	if tenantStore.Size() > 0 {
		alertSink := buildAbuseAlertSink(cfg.Abuse, billingSink)
		rl := auth.NewRateLimiter(
			auth.TenantBudgetsLookup(tenantStore),
			auth.TenantResolverFromAuth(authenticator),
		)
		rl.AlertSink = alertSink
		applyAbuseConfigToRateLimiter(rl, cfg.Abuse)
		// The abuse guard layers per-tenant egress bandwidth
		// budgets, 2x-of-baseline anomaly detection, and the
		// CDN-shielding gate in front of the S3 handler. It
		// shares the authenticator's tenant view with the rate
		// limiter so both guards see the same identity.
		ag := auth.NewAbuseGuard(
			auth.TenantLookupFromStore(tenantStore),
			auth.TenantResolverFromAuth(authenticator),
		)
		ag.AlertSink = alertSink
		applyAbuseConfigToAbuseGuard(ag, cfg.Abuse)
		handler = ag.Middleware(rl.Middleware(mux))
	}

	srv := &http.Server{
		Addr:         cfg.Gateway.ListenAddr,
		Handler:      handler,
		ReadTimeout:  cfg.Gateway.ReadTimeout.ToDuration(),
		WriteTimeout: cfg.Gateway.WriteTimeout.ToDuration(),
	}

	// Console API: separate HTTP surface for the tenant console
	// (react frontend in frontend/). It runs on its own listener
	// so a saturated S3 data plane cannot starve the management
	// controls operators use to diagnose it. The default address
	// is :8081 when the operator has not overridden it in config.
	consoleSrv := startConsoleAPI(cfg, metadataDB, tenantStore, authStore, authHooks, billingSink, billingProvider)

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
		if consoleSrv != nil {
			consoleCtx, consoleCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := consoleSrv.Shutdown(consoleCtx); err != nil {
				log.Printf("gateway: console shutdown: %v", err)
			}
			consoleCancel()
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
	if orphanGCDone != nil {
		<-orphanGCDone
	}
}

// startOrphanGC spins up the content_index orphan sweep when
// dedup is enabled and an interval is configured. The sweep
// removes content_index rows whose piece is no longer referenced
// by any live manifest in the tenant. Returns nil when no GC is
// started.
func startOrphanGC(
	ctx context.Context,
	d config.DedupConfig,
	idx content_index.Store,
	store manifest_store.ManifestStore,
	registry map[string]providers.StorageProvider,
) <-chan struct{} {
	if !d.Enabled || d.OrphanGCInterval.ToDuration() <= 0 {
		return nil
	}
	if idx == nil {
		log.Printf("gateway: orphan_gc disabled — content index not configured")
		return nil
	}
	resolver := func(backend string) (content_index.PieceDeleter, bool) {
		p, ok := registry[backend]
		if !ok {
			return nil, false
		}
		return p, true
	}
	gc, err := content_index.NewOrphanGC(content_index.OrphanGCConfig{
		Index:     idx,
		Manifests: store,
		Resolver:  resolver,
		Interval:  d.OrphanGCInterval.ToDuration(),
		Logger:    log.New(os.Stdout, "orphan_gc ", log.LstdFlags),
	})
	if err != nil {
		log.Printf("gateway: orphan_gc disabled — config error: %v", err)
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		gc.Run(ctx)
	}()
	return done
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

// applyDBConnectionPool applies the gateway's RDS / Postgres
// connection-pool tuning from cfg.ControlPlane to the shared
// metadata *sql.DB. The gateway opens exactly one *sql.DB per
// metadata DSN (see openMetadataDB) and shares it across every
// Postgres-backed store, so MaxOpenConns is the global ceiling
// for the gateway's metadata connection count, not a per-pool
// multiplier. Unset (zero-valued) fields leave Go's stdlib
// defaults in place.
func applyDBConnectionPool(db *sql.DB, cfg config.ControlPlaneConfig) {
	if cfg.MaxOpenConns > 0 {
		db.SetMaxOpenConns(cfg.MaxOpenConns)
	}
	if cfg.MaxIdleConns > 0 {
		db.SetMaxIdleConns(cfg.MaxIdleConns)
	}
	if d := time.Duration(cfg.ConnMaxLifetime); d > 0 {
		db.SetConnMaxLifetime(d)
	}
	if d := time.Duration(cfg.ConnMaxIdleTime); d > 0 {
		db.SetConnMaxIdleTime(d)
	}
}

// openMetadataDB opens the single shared *sql.DB the gateway uses
// for every Postgres-backed store (manifest, auth, dedicated cell,
// placement, tenant). All five stores share this pool so the
// connection-count ceiling configured in
// ControlPlaneConfig.MaxOpenConns is the gateway-process-wide cap,
// not a per-store multiplier. Returns (nil, nil) when MetadataDSN
// is empty (dev / in-memory mode) so callers can branch on db ==
// nil without inspecting the config again.
func openMetadataDB(cfg config.Config) (*sql.DB, error) {
	if cfg.ControlPlane.MetadataDSN == "" {
		return nil, nil
	}
	db, err := sql.Open("postgres", cfg.ControlPlane.MetadataDSN)
	if err != nil {
		return nil, fmt.Errorf("open postgres metadata DB: %w", err)
	}
	applyDBConnectionPool(db, cfg.ControlPlane)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres metadata DB: %w", err)
	}
	return db, nil
}

// buildContentIndex returns the intra-tenant deduplication index
// store. Postgres-backed when MetadataDSN is configured AND the
// dedup feature is enabled in cfg.Dedup; in-memory otherwise. The
// in-memory store is process-local and loses every entry on
// restart, so it MUST NOT be used in production with dedup enabled.
//
// When cfg.Dedup.Enabled is false the function returns nil so the
// S3 handler short-circuits the dedup path: every PUT writes a
// fresh piece, every DELETE removes it directly, and the store is
// never consulted.
func buildContentIndex(cfg config.Config, db *sql.DB) content_index.Store {
	if !cfg.Dedup.Enabled {
		log.Printf("gateway: dedup disabled (dedup.enabled = false); content_index store will not be built")
		return nil
	}
	if db == nil {
		log.Printf("gateway: dedup enabled with no metadata_dsn; using in-memory content_index store (dev only — entries do NOT survive restart)")
		return content_index.NewMemoryStore()
	}
	store, err := cipostgres.New(cipostgres.Config{DB: db})
	if err != nil {
		log.Fatalf("gateway: build postgres content_index store: %v", err)
	}
	log.Printf("gateway: postgres content_index store enabled (default_scope=%s default_level=%s)", cfg.Dedup.DefaultScope, cfg.Dedup.DefaultLevel)
	return store
}

func buildManifestStore(cfg config.Config, db *sql.DB) manifest_store.ManifestStore {
	if db == nil {
		log.Printf("gateway: no control_plane.metadata_dsn; using in-memory manifest store (dev only)")
		return memory.New()
	}
	pgCfg := pgstore.Config{DB: db}
	if p := cfg.Encryption.ManifestBodyKeyPath; p != "" {
		key, rerr := os.ReadFile(p)
		if rerr != nil {
			log.Fatalf("gateway: read manifest body key %q: %v", p, rerr)
		}
		enc, eerr := pgstore.NewAEADBodyEncryptor(key)
		if eerr != nil {
			log.Fatalf("gateway: build manifest body encryptor: %v", eerr)
		}
		pgCfg.BodyEncryptor = enc
		log.Printf("gateway: manifest body encryption enabled (key=%s)", p)
	}
	store, err := pgstore.New(pgCfg)
	if err != nil {
		log.Fatalf("gateway: build postgres manifest store: %v", err)
	}
	return store
}

// buildGatewayEncryption constructs the GatewayEncryption wiring
// the S3 handler consumes for managed / public_distribution
// tenant policies. The wrapper is selected from cfg.CMKURI:
//
//   - "" or "cmk://local/..."   → LocalFileWrapper (Phase 2 default;
//     plaintext master key on disk; suitable for dev only).
//   - "arn:aws:kms:..." or "kms://..." → KMSWrapper backed by AWS KMS.
//   - "vault://..." or "transit://..." → VaultWrapper backed by
//     HashiCorp Vault's Transit engine.
//
// Returns nil when no CMK is configured, which forces managed /
// public_distribution tenant policies to fail closed at PUT time
// rather than silently degrade to plaintext storage.
func buildGatewayEncryption(cfg config.EncryptionConfig) *s3compat.GatewayEncryption {
	uri := cfg.CMKURI
	if uri == "" && cfg.CMKPath != "" {
		uri = "cmk://local/" + cfg.CMKPath
	}
	if uri == "" {
		log.Printf("gateway: no encryption.cmk_uri / cmk_path set; managed / public_distribution tenant policies will fail with EncryptionNotConfigured")
		return nil
	}

	wrapper, holderClass, err := selectGatewayWrapper(cfg, uri)
	if err != nil {
		log.Fatalf("gateway: build encryption wrapper: %v", err)
	}
	log.Printf("gateway: encryption configured (uri=%s holder=%s)", uri, holderClass)
	return &s3compat.GatewayEncryption{
		Wrapper: wrapper,
		CMK: encryption.CustomerMasterKeyRef{
			URI:         uri,
			Version:     1,
			HolderClass: holderClass,
		},
	}
}

// selectGatewayWrapper returns the client_sdk.Wrapper bound to uri
// plus the encryption.CustomerMasterKeyRef.HolderClass tag the
// manifest records on every wrapped DEK. The tag drives runbook
// decisions when an operator inspects a manifest in the field.
func selectGatewayWrapper(cfg config.EncryptionConfig, uri string) (client_sdk.Wrapper, string, error) {
	switch {
	case strings.HasPrefix(uri, "arn:aws:kms:"), strings.HasPrefix(uri, "kms://"):
		region := cfg.KMSRegion
		if region == "" {
			region = os.Getenv("AWS_REGION")
		}
		ctx := context.Background()
		opts := []func(*awsconfig.LoadOptions) error{}
		if region != "" {
			opts = append(opts, awsconfig.WithRegion(region))
		}
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			return nil, "", fmt.Errorf("load aws config: %w", err)
		}
		client := kms.NewFromConfig(awsCfg)
		return client_sdk.NewKMSWrapper(client), "aws_kms", nil

	case strings.HasPrefix(uri, "vault://"), strings.HasPrefix(uri, "transit://"):
		addr := cfg.VaultAddr
		if addr == "" {
			addr = os.Getenv("VAULT_ADDR")
		}
		token := cfg.VaultToken
		if token == "" {
			token = os.Getenv("VAULT_TOKEN")
		}
		if addr == "" || token == "" {
			return nil, "", fmt.Errorf("vault wrapper requires VAULT_ADDR/VAULT_TOKEN (set encryption.vault_addr / vault_token in config or the env vars)")
		}
		mount := cfg.VaultTransitMount
		if mount == "" {
			mount = client_sdk.DefaultVaultMount
		}
		return client_sdk.NewVaultWrapper(addr, token, mount), "vault_transit", nil

	default:
		// Default and "cmk://local/..." fall through to the
		// local-file wrapper. Fail loudly when CMKPath is empty
		// so operators don't accidentally ship without a usable
		// wrapper.
		if cfg.CMKPath == "" {
			return nil, "", fmt.Errorf("local file wrapper requires encryption.cmk_path when cmk_uri is %q", uri)
		}
		return client_sdk.LocalFileWrapper{Path: cfg.CMKPath}, "gateway_hsm", nil
	}
}

// buildAbuseAlertSink composes the abuse / rate-limit alert sink.
// The billing sink is always wired (durable metering) and an
// optional WebhookAlertSink is fanned out alongside it when
// cfg.AlertWebhookURL is set.
func buildAbuseAlertSink(cfg config.AbuseConfig, billingSink auth.AlertSink) auth.AlertSink {
	if cfg.AlertWebhookURL == "" {
		return billingSink
	}
	webhook := auth.NewWebhookAlertSink(cfg.AlertWebhookURL)
	webhook.Logger = log.New(os.Stdout, "abuse_webhook ", log.LstdFlags)
	log.Printf("gateway: abuse alert webhook enabled (%s)", cfg.AlertWebhookURL)
	return auth.NewMultiAlertSink(billingSink, webhook)
}

// applyAbuseConfigToAbuseGuard copies the per-region runtime knobs
// from cfg onto the abuse guard. Zero values fall through so the
// in-package defaults remain authoritative when an operator has
// not customized them.
func applyAbuseConfigToAbuseGuard(g *auth.AbuseGuard, cfg config.AbuseConfig) {
	if cfg.AnomalyMultiplier > 0 {
		g.AnomalyMultiplier = cfg.AnomalyMultiplier
	}
	if cfg.AnomalyWindow > 0 {
		g.AnomalyWindow = cfg.AnomalyWindow.ToDuration()
	}
	if cfg.AnomalyCooldown > 0 {
		g.AnomalyCooldown = cfg.AnomalyCooldown.ToDuration()
	}
	if cfg.BaselineAlpha > 0 {
		g.BaselineAlpha = cfg.BaselineAlpha
	}
	g.ThrottleOnAnomaly = cfg.ThrottleOnAnomaly
}

// applyAbuseConfigToRateLimiter copies the same per-region runtime
// knobs onto the rate limiter so abuse guard and rate limiter
// share the same baseline EWMA / multiplier / cooldown semantics.
func applyAbuseConfigToRateLimiter(l *auth.RateLimiter, cfg config.AbuseConfig) {
	if cfg.AnomalyMultiplier > 0 {
		l.AnomalyMultiplier = cfg.AnomalyMultiplier
	}
	if cfg.AnomalyWindow > 0 {
		l.AnomalyWindow = cfg.AnomalyWindow.ToDuration()
	}
	if cfg.AnomalyCooldown > 0 {
		l.AnomalyCooldown = cfg.AnomalyCooldown.ToDuration()
	}
	if cfg.BaselineAlpha > 0 {
		l.BaselineAlpha = cfg.BaselineAlpha
	}
	l.ThrottleOnAnomaly = cfg.ThrottleOnAnomaly
}

func buildProviderRegistry(ctx context.Context, cfg config.Config) map[string]providers.StorageProvider {
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
	// Per-region Wasabi providers (Phase 3 multi-region).
	// Each region registers under its ResolvedName() so placement
	// policies can target e.g. "wasabi-us-east-1" or
	// "wasabi-eu-central-1" explicitly.
	for _, r := range cfg.Providers.Wasabi.Regions {
		if r.Endpoint == "" || r.Bucket == "" {
			continue
		}
		name := r.ResolvedName()
		if _, exists := registry[name]; exists {
			log.Fatalf("gateway: duplicate wasabi region name %q", name)
		}
		w, err := wasabi.New(wasabi.Config{
			Endpoint:  r.Endpoint,
			Region:    r.Region,
			Bucket:    r.Bucket,
			AccessKey: r.AccessKey,
			SecretKey: r.SecretKey,
		})
		if err != nil {
			log.Fatalf("gateway: build wasabi region %q: %v", name, err)
		}
		registry[name] = w
		log.Printf("gateway: registered wasabi region provider %q (endpoint=%s bucket=%s)", name, r.Endpoint, r.Bucket)
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
	if cfg.Providers.BackblazeB2.Endpoint != "" {
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
	if cfg.Providers.CloudflareR2.AccountID != "" || cfg.Providers.CloudflareR2.Endpoint != "" {
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
	if cfg.Providers.AWSS3.Region != "" {
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
	if cfg.Providers.Storj.AccessGrant != "" {
		project, err := storj.OpenUplinkProject(ctx, storj.Config{
			AccessGrant:      cfg.Providers.Storj.AccessGrant,
			Bucket:           cfg.Providers.Storj.Bucket,
			SatelliteAddress: cfg.Providers.Storj.SatelliteAddress,
		})
		if err != nil {
			log.Fatalf("gateway: open storj uplink: %v", err)
		}
		s, err := storj.NewWithUplink(storj.Config{
			AccessGrant:      cfg.Providers.Storj.AccessGrant,
			Bucket:           cfg.Providers.Storj.Bucket,
			SatelliteAddress: cfg.Providers.Storj.SatelliteAddress,
		}, project)
		if err != nil {
			log.Fatalf("gateway: build storj: %v", err)
		}
		registry["storj"] = s
	}
	return registry
}

// pickDefaultBackend returns the first backend name in a stable
// preference order so the gateway boots with a usable placement
// default even without explicit tenant policies. The "wasabi" slot
// also matches multi-region keys of the form "wasabi-<region>"
// (registered via WasabiConfig.Regions). When several wasabi-*
// providers are registered we pick the lexicographically smallest
// key for determinism — operators that need a different default
// should set an explicit tenant placement policy.
func pickDefaultBackend(registry map[string]providers.StorageProvider) string {
	for _, name := range []string{"wasabi", "ceph_rgw", "backblaze_b2", "cloudflare_r2", "aws_s3", "storj", "local_fs_dev"} {
		if _, ok := registry[name]; ok {
			return name
		}
		if name == "wasabi" {
			if region := firstWasabiRegionKey(registry); region != "" {
				return region
			}
		}
	}
	return ""
}

// firstWasabiRegionKey returns the lexicographically smallest
// "wasabi-<region>" entry registered in registry, or "" when no
// such entry exists. Sorting keeps the boot-time default stable
// across restarts; Go map iteration order is randomized.
func firstWasabiRegionKey(registry map[string]providers.StorageProvider) string {
	var keys []string
	for k := range registry {
		if strings.HasPrefix(k, "wasabi-") {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	return keys[0]
}

// buildHotObjectCache returns a DiskCache when cfg.Gateway.CachePath
// is set and a writable directory, falling back to an in-memory
// cache for developer/test flows. The cache capacity is 1 GiB so
// small dev machines don't fill the disk; operators size it via
// the eviction policy in Phase 4's config refactor.
//
// Production nodes configure cache_path so the NVMe tier survives
// gateway restarts. Dev and CI environments leave it unset and get
// an in-memory cache. When cache_path is set but the disk cache
// cannot be opened (permission error, missing volume, corrupt
// warm-up), we log a warning and degrade to the memory cache so a
// single bad disk doesn't knock the gateway offline.
func buildHotObjectCache(cfg config.Config) (hot_object_cache.HotObjectCache, error) {
	policy := hot_object_cache.DefaultEvictionPolicy(1 << 30)
	if cfg.Gateway.CachePath == "" {
		return hot_object_cache.NewMemoryCache(policy)
	}
	disk, err := hot_object_cache.NewDiskCache(hot_object_cache.DiskCacheConfig{
		RootPath: cfg.Gateway.CachePath,
		Policy:   policy,
	})
	if err != nil {
		log.Printf("gateway: disk cache at %q unavailable (%v); falling back to in-memory cache", cfg.Gateway.CachePath, err)
		return hot_object_cache.NewMemoryCache(policy)
	}
	return disk, nil
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

// buildBillingProvider resolves the configured BillingProvider via
// billing.BuildProvider. An empty cfg.Billing.Provider falls back to
// the no-op provider so the gateway boots without an outbound
// integration. Plug-ins (Stripe, Chargebee, …) register themselves
// at init() time via billing.RegisterProvider; the gateway does not
// import vendor packages directly.
func buildBillingProvider(cfg config.Config) billing.BillingProvider {
	provider, err := billing.BuildProvider(billing.ProviderFactoryConfig{
		Name:     cfg.Billing.Provider,
		Settings: cfg.Billing.ProviderConfig,
		Logger:   log.New(os.Stdout, "billing.provider ", log.LstdFlags),
	})
	if err != nil {
		log.Fatalf("gateway: build billing provider: %v", err)
	}
	log.Printf("gateway: billing provider %q wired", provider.Name())
	return provider
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

func startConsoleAPI(
	cfg config.Config,
	metadataDB *sql.DB,
	tenantStore auth.TenantStore,
	authStore console.AuthStore,
	authHooks console.AuthHooks,
	billingSink billing.BillingSink,
	billingProvider billing.BillingProvider,
) *http.Server {
	if cfg.Console.ListenAddr == "" {
		return nil
	}
	// Adapter: LookupTenant returns only the Tenant value so the
	// secret key stays inside the auth package. AddAPIKey wires
	// new bindings straight back into the tenant store the
	// authenticator already consults.
	tenants := &consoleTenantAdapter{store: tenantStore}
	// Usage adapter: if the billing sink is a ClickHouse sink it
	// satisfies console.UsageQuery directly; otherwise we hand
	// the console a no-op stub that returns an empty map so the
	// frontend still renders a dashboard shell.
	var usage console.UsageQuery = noopUsageQuery{}
	if uq, ok := billingSink.(console.UsageQuery); ok {
		usage = uq
	}
	placements := buildPlacementStore(metadataDB)
	tokens := console.NewMemoryTokenStore()

	cellStore := buildDedicatedCellStore(metadataDB)
	cellProvisioner := buildCellProvisioner(cellStore)

	h := console.New(console.Config{
		Tenants:         tenants,
		Usage:           usage,
		Placements:      placements,
		Auth:            authStore,
		Tokens:          tokens,
		AuthHooks:       authHooks,
		AdminAuth:       buildAdminAuth(cfg),
		BillingSink:     billingSink,
		BillingProvider: billingProvider,
		Buckets:         console.NewMemoryBucketStore(),
		Cells:           cellStore,
		CellProvisioner: cellProvisioner,
		DedupPolicies:   console.NewMemoryDedupPolicyStore(),
	})
	mux := http.NewServeMux()
	h.Register(mux)

	srv := &http.Server{
		Addr:         cfg.Console.ListenAddr,
		Handler:      mux,
		ReadTimeout:  cfg.Console.ReadTimeout.ToDuration(),
		WriteTimeout: cfg.Console.WriteTimeout.ToDuration(),
	}
	go func() {
		log.Printf("gateway: console API on %s", cfg.Console.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("gateway: console listener: %v", err)
		}
	}()
	return srv
}

// buildAuthStore returns the Postgres-backed AuthStore when a
// metadata DSN is configured, falling back to MemoryAuthStore for
// dev mode. The store is shared between the console signup / login
// handler and the S3 handler's email-verification gate.
func buildAuthStore(db *sql.DB) console.AuthStore {
	if db == nil {
		return console.NewMemoryAuthStore()
	}
	store, err := console.NewPostgresAuthStore(db)
	if err != nil {
		log.Printf("gateway: build postgres auth store: %v; falling back to in-memory", err)
		return console.NewMemoryAuthStore()
	}
	log.Printf("gateway: postgres auth store enabled")
	return store
}

// buildDedicatedCellStore returns the Postgres-backed cell store
// when a metadata DSN is configured, falling back to the in-memory
// store for dev. Production wires this so console-driven cell
// provisioning requests persist across gateway restarts.
func buildDedicatedCellStore(db *sql.DB) console.DedicatedCellStore {
	if db == nil {
		return console.NewMemoryDedicatedCellStore()
	}
	store, err := console.NewPostgresDedicatedCellStore(db)
	if err != nil {
		log.Printf("gateway: build postgres dedicated cell store: %v; falling back to in-memory", err)
		return console.NewMemoryDedicatedCellStore()
	}
	log.Printf("gateway: postgres dedicated cell store enabled")
	return store
}

// buildCellProvisioner returns a ManualProvisioner backed by store
// when store satisfies cellops.CellSink. The Phase 3 in-memory and
// Postgres dedicated-cell stores both do; a future custom store
// that does not implement CellSink simply gets a nil provisioner
// and the POST /dedicated-cells endpoint reports
// 503 service unavailable.
func buildCellProvisioner(store console.DedicatedCellStore) cellops.CellProvisioner {
	sink, ok := store.(cellops.CellSink)
	if !ok {
		return nil
	}
	prov := cellops.NewManualProvisioner(sink)
	prov.Logger = log.New(os.Stdout, "cellops ", log.LstdFlags)
	return prov
}

// buildAdminAuth returns a bearer-token verifier when cfg.Console
// AdminToken is set, or nil when it is not (dev mode). The token is
// a shared secret; the check is a constant-time comparison so a
// malformed header can't leak timing information about the stored
// value.
func buildAdminAuth(cfg config.Config) func(r *http.Request) bool {
	token := cfg.Console.AdminToken
	if token == "" {
		log.Printf("gateway: console admin_token not set; console API is unauthenticated (dev only)")
		return nil
	}
	expected := []byte("Bearer " + token)
	return func(r *http.Request) bool {
		got := []byte(r.Header.Get("Authorization"))
		return len(got) == len(expected) && subtle.ConstantTimeCompare(got, expected) == 1
	}
}

// buildAuthHooks wires the hCaptcha / SES hooks when their
// configuration secrets are available via the environment. Phase 3
// hooks fall back to no-ops so dev / test deploys still work without
// a hCaptcha site secret or an AWS SES account.
func buildAuthHooks(cfg config.Config) console.AuthHooks {
	hooks := console.AuthHooks{}
	// Config-driven CAPTCHA wiring takes precedence over the env
	// fallback so operators can flip the provider without
	// redeploying the gateway. Only hcaptcha is wired today;
	// "recaptcha" is reserved for a future provider and currently
	// logs a warning and falls through to env so an unset config
	// does not silently disable CAPTCHA.
	captchaSecret := cfg.Console.CaptchaSecret
	captchaProvider := strings.ToLower(strings.TrimSpace(cfg.Console.CaptchaProvider))
	if captchaSecret == "" {
		captchaSecret = os.Getenv("HCAPTCHA_SECRET")
	}
	switch captchaProvider {
	case "", "hcaptcha":
		if captchaSecret != "" {
			hooks.VerifyCAPTCHA = console.NewHCaptchaVerifier(captchaSecret, "")
		}
	case "recaptcha":
		log.Printf("gateway: console captcha_provider=recaptcha is not wired yet; signup CAPTCHA disabled")
	default:
		log.Printf("gateway: unknown console captcha_provider %q; signup CAPTCHA disabled", captchaProvider)
	}
	if from := os.Getenv("SES_FROM_ADDRESS"); from != "" {
		if sender, err := console.NewSESEmailSender(console.SESEmailConfig{
			FromAddress:   from,
			Region:        os.Getenv("AWS_REGION"),
			VerifyBaseURL: os.Getenv("CONSOLE_VERIFY_BASE_URL"),
		}); err == nil {
			hooks.SendVerificationEmail = sender
		} else {
			log.Printf("gateway: build SES verification email sender: %v", err)
		}
	}
	return hooks
}

// buildPlacementStore returns the Postgres-backed PlacementStore when
// cfg.ControlPlane.MetadataDSN is set, or an in-memory store for dev.
// The Postgres store reuses the same DSN as the manifest store; the
// schema migration lives in api/console/schema.sql.
func buildPlacementStore(db *sql.DB) console.PlacementStore {
	if db == nil {
		return console.NewMemoryPlacementStore()
	}
	store, err := console.NewPostgresPlacementStore(db)
	if err != nil {
		log.Printf("gateway: build postgres placement store: %v; falling back to in-memory", err)
		return console.NewMemoryPlacementStore()
	}
	return store
}

// consoleTenantAdapter bridges auth.TenantStore to
// console.TenantStore. It lives in main so the auth package does
// not have to know about the console API.
type consoleTenantAdapter struct {
	store auth.TenantStore
}

func (c *consoleTenantAdapter) LookupTenant(tenantID string) (tenant.Tenant, bool) {
	b, ok := c.store.LookupByTenantID(tenantID)
	if !ok {
		return tenant.Tenant{}, false
	}
	return b.Tenant, true
}

func (c *consoleTenantAdapter) AddAPIKey(tenantID, accessKey, secretKey string) error {
	b, ok := c.store.LookupByTenantID(tenantID)
	if !ok {
		return fmt.Errorf("gateway: tenant %q not found", tenantID)
	}
	// Reject duplicate access keys. MemoryTenantStore.AddBinding
	// silently replaces on collision, which would let a console
	// caller overwrite the secret for an access key that already
	// authenticates a different (or the same) tenant — a silent
	// credential swap the console API must not enable.
	if _, exists := c.store.LookupByAccessKey(accessKey); exists {
		return fmt.Errorf("gateway: access key %q is already bound", accessKey)
	}
	return c.store.AddBinding(auth.TenantBinding{
		AccessKey: accessKey,
		SecretKey: secretKey,
		Tenant:    b.Tenant,
	})
}

// CreateTenant forwards to the backing MemoryTenantStore so the
// console signup handler can register a new tenant record before
// minting its initial API key pair.
func (c *consoleTenantAdapter) CreateTenant(t tenant.Tenant) error {
	return c.store.CreateTenant(t)
}

// DeleteTenant forwards to the backing MemoryTenantStore so the
// console signup handler can roll back a partially-created tenant
// when a downstream step (CreateUser, AddAPIKey, IssueToken) fails.
func (c *consoleTenantAdapter) DeleteTenant(tenantID string) error {
	return c.store.DeleteTenant(tenantID)
}

// bindingLister is satisfied by both MemoryTenantStore and
// PostgresTenantStore. It is type-asserted dynamically so mock
// stores used in tests remain usable without implementing the
// key-management surface.
type bindingLister interface {
	ListBindingsByTenantID(tenantID string) ([]auth.TenantBinding, error)
	RemoveBinding(accessKey string) error
}

// ListAPIKeys implements console.APIKeyLister. It returns the
// tenant's access-key bindings (without the secret key) so the
// console UI can render a keys table without ever exposing the
// secret back to the browser.
func (c *consoleTenantAdapter) ListAPIKeys(tenantID string) ([]console.APIKeyDescriptor, error) {
	bl, ok := c.store.(bindingLister)
	if !ok {
		return nil, fmt.Errorf("gateway: tenant store does not expose binding listing")
	}
	bindings, err := bl.ListBindingsByTenantID(tenantID)
	if err != nil {
		return nil, fmt.Errorf("gateway: list bindings for tenant %q: %w", tenantID, err)
	}
	out := make([]console.APIKeyDescriptor, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, console.APIKeyDescriptor{
			AccessKey: b.AccessKey,
			// The binding does not persist its own CreatedAt
			// so fall back to the zero value; the SPA renders
			// "unknown" when the timestamp is zero. A later
			// schema migration can populate this from the
			// tenant_bindings row.
		})
	}
	return out, nil
}

// DeleteAPIKey implements console.APIKeyLister by removing the
// binding so the access key stops authenticating S3 requests on
// the next request (the auth store is the authoritative hot path).
func (c *consoleTenantAdapter) DeleteAPIKey(tenantID, accessKey string) error {
	bl, ok := c.store.(bindingLister)
	if !ok {
		return fmt.Errorf("gateway: tenant store does not expose binding removal")
	}
	b, ok := c.store.LookupByAccessKey(accessKey)
	if !ok {
		return nil // idempotent
	}
	if b.Tenant.ID != tenantID {
		return fmt.Errorf("gateway: access key %q is not bound to tenant %q", accessKey, tenantID)
	}
	return bl.RemoveBinding(accessKey)
}

// noopUsageQuery is the zero-cost fallback used when no ClickHouse
// sink is wired. It returns an empty counter map so the frontend
// renders a dashboard shell even in local development.
type noopUsageQuery struct{}

func (noopUsageQuery) TenantUsage(ctx context.Context, tenantID string, start, end time.Time) (map[string]uint64, error) {
	return map[string]uint64{}, nil
}

// buildTenantStore returns the Postgres-backed tenant store when
// cfg.ControlPlane.MetadataDSN is set, or the in-memory store for
// dev. The in-memory store additionally loads bindings from a JSON
// file when path is supplied; the Postgres path ignores the JSON
// file because production deploys load bindings via the console API
// signup flow.
func buildTenantStore(db *sql.DB, path string) auth.TenantStore {
	if db != nil {
		store, err := auth.NewPostgresTenantStore(db)
		if err != nil {
			log.Fatalf("gateway: build postgres tenant store: %v", err)
		}
		if path != "" {
			log.Printf("gateway: --tenants flag ignored when control_plane.metadata_dsn is set")
		}
		return store
	}
	store := auth.NewMemoryTenantStore()
	if path == "" {
		return store
	}
	if err := store.LoadBindingsFromJSON(path); err != nil {
		log.Fatalf("gateway: load tenants: %v", err)
	}
	return store
}
