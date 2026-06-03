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
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/kms"

	"github.com/kennguy3n/zk-object-fabric/api/console"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat"
	"github.com/kennguy3n/zk-object-fabric/api/s3compat/multipart"
	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/cache/hot_object_cache"
	"github.com/kennguy3n/zk-object-fabric/encryption"
	"github.com/kennguy3n/zk-object-fabric/encryption/client_sdk"
	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/cellops"
	"github.com/kennguy3n/zk-object-fabric/internal/compliance"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/internal/health"
	"github.com/kennguy3n/zk-object-fabric/internal/metrics"
	"github.com/kennguy3n/zk-object-fabric/internal/repair"
	"github.com/kennguy3n/zk-object-fabric/internal/requestid"
	"github.com/kennguy3n/zk-object-fabric/internal/tracing"
	"github.com/kennguy3n/zk-object-fabric/internal/version"
	"github.com/kennguy3n/zk-object-fabric/metadata/content_index"
	cipostgres "github.com/kennguy3n/zk-object-fabric/metadata/content_index/postgres"
	cisqlite "github.com/kennguy3n/zk-object-fabric/metadata/content_index/sqlite"
	"github.com/kennguy3n/zk-object-fabric/metadata/erasure_coding"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/memory"
	pgstore "github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/postgres"
	sqlitestore "github.com/kennguy3n/zk-object-fabric/metadata/manifest_store/sqlite"
	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
	"github.com/kennguy3n/zk-object-fabric/migration"
	"github.com/kennguy3n/zk-object-fabric/migration/background_rebalancer"
	"github.com/kennguy3n/zk-object-fabric/migration/cross_cell"
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
	// allowLocalCMK is the CLI override for
	// EncryptionConfig.AllowLocalCMK. The default is the
	// zero value (false) so the flag's mere presence (when
	// the operator passes `--allow-local-cmk`) flips the
	// gateway out of fail-closed mode for the local-file CMK
	// wrapper. The CLI flag wins over the JSON config when
	// both are set so operators can flip the behaviour at
	// run-time without rewriting their config file.
	allowLocalCMK := flag.Bool("allow-local-cmk", false, "permit env=production with cmk://local/... — escape hatch for HSM-fuse deployments where the local file path maps to hardware-backed key material; do NOT set this against a plaintext key file (overrides encryption.allow_local_cmk)")
	flag.Parse()

	cfg := config.Default()
	if *cfgPath != "" {
		loaded, err := config.Load(*cfgPath)
		if err != nil {
			log.Fatalf("gateway: load config: %v", err)
		}
		cfg = loaded
	}
	// CLI flag wins over JSON config when both are set.
	if *allowLocalCMK {
		cfg.Encryption.AllowLocalCMK = true
	}

	// Cross-field config validation runs AFTER CLI overrides so
	// the error message reflects the effective configuration the
	// gateway is actually about to use (a flag that overrode a
	// JSON value into an inconsistent state would otherwise slip
	// past). The current checks catch operational footguns the
	// type system cannot — e.g. read_header_timeout >
	// read_timeout would silently nullify the Slowloris defence
	// because ReadTimeout fires first. Refusing to start is the
	// right failure mode here; a degraded edge defence is worse
	// than no edge defence because it gives a false sense of
	// security in the runbook.
	if err := cfg.Validate(); err != nil {
		log.Fatalf("gateway: %v", err)
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

	// Embedded / single-node profile: when no Postgres DSN is set
	// but an embedded DB path is configured, open the local SQLite
	// database the manifest, content-index, auth, and billing
	// stores share. metadataDB stays nil in this mode (the
	// Postgres-only stores fall back to their in-memory dev
	// variants); embeddedDB is threaded into the four stores that
	// have a SQLite backend. See openEmbeddedDB.
	embeddedDB, err := openEmbeddedDB(cfg)
	if err != nil {
		log.Fatalf("gateway: %v", err)
	}
	if embeddedDB != nil {
		defer func() { _ = embeddedDB.Close() }()
	}

	// Pre-flight TLS validation for every configured listener
	// BEFORE any goroutines spawn. startListener also calls
	// t.Validate inside as a defence-in-depth, but the console
	// and health listeners run their startListener call inside
	// a goroutine — a validation error there would only
	// log.Printf and silently disable the listener instead of
	// failing the process. Doing the validation here means a
	// partial TLS config (cert_path set but key_path empty, or
	// an unsupported min_version) on ANY listener fails the
	// startup synchronously with log.Fatalf, which is the right
	// failure mode for a production deployment that intended
	// TLS but typoed one of the paths.
	validateAllTLSConfigs(cfg)

	store := buildManifestStore(cfg, metadataDB, embeddedDB)
	contentIndex := buildContentIndex(cfg, metadataDB, embeddedDB)
	registry := buildProviderRegistry(context.Background(), cfg)
	if lister, ok := buildDedicatedCellStore(metadataDB).(cellops.CellLister); ok {
		registerCellProviders(context.Background(), registry, lister, cfg)
	}
	defaultBackend := pickDefaultBackend(registry)
	if defaultBackend == "" {
		log.Fatalf("gateway: no storage providers registered; configure at least one in config.providers")
	}

	placement := placement_policy.NewEngine(defaultBackend, registry, nil)
	tenantStore := buildTenantStore(metadataDB, *tenantsPath)
	// Production safety net: refuse to start when the gateway is
	// wired with the dev in-memory tenant store and no static
	// --tenants bindings have been loaded. This is layered
	// alongside the s3compat handler's RequireAuth check
	// downstream so neither a missing Auth field nor a missing
	// tenant store can leave an effectively-unauthenticated
	// production handler in place.
	enforceProductionAuth(cfg.Env, metadataDB, tenantStore)
	// Production safety net: refuse to start when env=production,
	// the gateway is wired with a persistent manifest store, and
	// the BodyEncryptor key is not configured. Without this,
	// manifest JSON (object keys, piece locators, sizes, wrapped
	// DEKs) is persisted as plaintext and any operator or attacker
	// with read access to that store can enumerate tenant content.
	// See manifest_store.BodyEncryptor for the per-row sealing logic
	// this guard refuses to skip.
	//
	// A persistent store is selected when metadataDB != nil (Postgres
	// JSONB) or embeddedDB != nil (embedded SQLite blob on disk); see
	// buildManifestStore. The in-memory dev store has no persistent
	// backing table to leak, so the guard skips that branch — if
	// env=production also implies the memory store,
	// enforceProductionAuth above will already have failed because
	// the in-memory tenant store cannot satisfy production auth
	// without static --tenants bindings.
	enforceProductionManifestEncryption(cfg.Env, metadataDB != nil || embeddedDB != nil, cfg.Encryption.ManifestBodyKeyPath)
	// Production safety net: refuse to start when env=production, the
	// console API is enabled, and no console JWT signing key is
	// configured. Without it the console falls back to the process-
	// local MemoryTokenStore, whose tokens are lost on restart and are
	// not shared across replicas behind a load balancer. Skipped when
	// the console is disabled (ListenAddr empty), since no TokenStore
	// is built then — see checkProductionTokenStore.
	enforceProductionTokenStore(cfg.Env, cfg.Console.ListenAddr, cfg.Console.JWTSigningKeyPath)
	authenticator := auth.NewHMACAuthenticator(tenantStore)
	metricsRegistry := metrics.NewRegistry()
	tracer := buildTracer(cfg.Tracing)

	billingSink := buildBillingSink(cfg, embeddedDB)
	if cfg.Metrics.Enabled {
		billingSink = metrics.NewMetricsBillingSink(billingSink, metricsRegistry)
	}
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
	authStore := buildAuthStore(metadataDB, embeddedDB)
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
	// Share the same adapter as the HTTP handler so repair-path
	// integrity observations land on the same Prometheus series
	// (zkof_integrity_failure_total / zkof_integrity_claim_unrecognized_total).
	// Without this, fetchPiece's preVerified shortcut would hide
	// any unrecognised-claim count discovered during repair.
	readRepair.IntegrityFailures = integrityFailureSink{r: metricsRegistry}

	rebalancerDone := startRebalancer(workerCtx, cfg.Rebalancer, store, registry)
	orphanGCDone := startOrphanGC(workerCtx, cfg.Dedup, contentIndex, store, registry)
	crossCellDone := startCrossCellReplicator(workerCtx, cfg.CrossCell, store, registry)

	healthMon := startHealthMonitor(workerCtx, cfg.Health, cache, cfg.Env)

	erasureRegistry := erasure_coding.DefaultRegistry()
	repairDone := startRepairQueue(workerCtx, cfg.Repair, store, registry, erasureRegistry)

	gatewayEnc := buildGatewayEncryption(cfg.Encryption)
	if gatewayEnc != nil {
		enforceProductionLocalCMK(cfg.Env, cfg.Encryption.AllowLocalCMK, gatewayEnc.CMK.URI, gatewayEnc.CMK.HolderClass)
	}
	multipartStore, multipartCloser := buildMultipartStore(metadataDB, gatewayEnc, registry)
	if multipartCloser != nil {
		defer multipartCloser()
	}

	mux := http.NewServeMux()
	if cfg.Metrics.Enabled {
		path := cfg.Metrics.Path
		if path == "" {
			path = "/internal/metrics"
		}
		mux.Handle(path, metricsRegistry.Handler())
	}
	// /internal/version surfaces the ldflags-stamped build
	// metadata so an orchestrator can match a running pod to a
	// git SHA without shelling into the container. The handler
	// has no auth requirement on the same rationale as
	// /internal/metrics: operators are expected to keep the
	// /internal/* prefix behind their cluster ingress and not
	// expose it to tenants.
	mux.Handle(version.Path, version.Handler())
	log.Printf("gateway: build version=%s commit=%s built=%s go=%s/%s",
		version.Version, version.GitCommit, version.BuildDate, runtime.GOOS, runtime.GOARCH)
	complianceHooks := buildComplianceHooks(cfg.Compliance, metadataDB)
	s3compat.New(s3compat.Config{
		Manifests:                store,
		Providers:                registry,
		Placement:                placement,
		Auth:                     authenticator,
		VerifiedCheck:            verifiedCheck,
		Billing:                  billingSink,
		Multipart:                multipartStore,
		ErasureCoding:            erasureRegistry,
		Cache:                    cache,
		CachePublisher:           signalBus,
		ReadRepair:               readRepair,
		Encryption:               gatewayEnc,
		ContentIndex:             contentIndex,
		Compliance:               complianceHooks,
		NodeID:                   cfg.Env,
		IntegrityFailures:        integrityFailureSink{r: metricsRegistry},
		MaxRequestBytes:          cfg.Gateway.MaxRequestBytes,
		CacheWarmingMemoryBudget: cfg.Gateway.CacheWarmingMemoryBudget,
		OnCacheWarmingBudgetExhausted: func(backend string, pieceSize int64) {
			metricsRegistry.IncCacheWarmingBudgetExhausted(backend, pieceSize)
		},
		// RequireAuth gates the handler's AnonymousTenant
		// fallback: in production any misconfiguration that
		// drops the authenticator returns 500
		// InternalAuthMisconfigured instead of silently writing
		// data under the anonymous tenant.
		RequireAuth: cfg.Env == "production",
	}).Register(mux)

	handler := http.Handler(mux)
	if cfg.Tracing.Enabled {
		handler = tracer.Middleware(handler)
	}
	if cfg.Metrics.Enabled {
		handler = metricsMiddleware(metricsRegistry, handler)
	}
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
		handler = ag.Middleware(rl.Middleware(handler))
	}
	// requestid.Middleware is the outermost layer so the
	// generated id is in the context before any other
	// middleware runs (rate limiter, abuse guard, metrics,
	// tracing). This makes the id available in every log line
	// every downstream handler emits, and also means a 429 from
	// the rate limiter still carries an x-amz-request-id header
	// so the client can quote it in a support ticket.
	handler = requestid.Middleware(handler)

	// ReadHeaderTimeout, IdleTimeout, and MaxHeaderBytes are the
	// gateway's primary defences against Slowloris-style
	// connection-exhaustion attacks. ReadTimeout alone is not
	// sufficient because it covers the *entire* request body, so
	// a client streaming a slow PUT can hold a goroutine open for
	// the full window. ReadHeaderTimeout bounds the cheaper
	// "header-stall" attack independently. See
	// tests/abuse/slowloris_test.go for the regression test that
	// pins these knobs.
	//
	// MaxHeaderBytes routes through config.EffectiveMaxHeaderBytes
	// so a hand-rolled config explicitly setting the field to 0
	// re-floors to config.DefaultMaxHeaderBytes (64 KiB) instead
	// of falling through to Go's stdlib 1 MiB default. This
	// collapses the explicit-0 and omitted-field paths so the
	// operator's diff is the only way to land at a different
	// effective ceiling. (Devin Review ANALYSIS_0001 on PR #80
	// flagged the prior 1-MiB fallback inconsistency.)
	warnIfSlowlorisDisabled("gateway", cfg.Gateway.ListenAddr, cfg.Gateway.ReadHeaderTimeout)
	gatewayMaxHeaderBytes := config.EffectiveMaxHeaderBytes(cfg.Gateway.MaxHeaderBytes)
	logEffectiveMaxHeaderBytes("gateway", cfg.Gateway.ListenAddr, gatewayMaxHeaderBytes)
	srv := &http.Server{
		Addr:              cfg.Gateway.ListenAddr,
		Handler:           handler,
		ReadTimeout:       cfg.Gateway.ReadTimeout.ToDuration(),
		WriteTimeout:      cfg.Gateway.WriteTimeout.ToDuration(),
		ReadHeaderTimeout: cfg.Gateway.ReadHeaderTimeout.ToDuration(),
		IdleTimeout:       cfg.Gateway.IdleTimeout.ToDuration(),
		MaxHeaderBytes:    gatewayMaxHeaderBytes,
	}

	// fleetOrchestrator coordinates large multi-tenant
	// migrations across many gateway nodes. Its JobStore is
	// PgJobStore when a metadata DSN is configured (so two
	// gateways serialise their claims through Postgres) and
	// InMemoryJobStore otherwise (single-node / dev). The
	// orchestrator surface is currently consumed only by the
	// management console's /api/v1/migrations endpoints;
	// future PRs add the enqueue and RunOnce-ticker wiring so
	// pending jobs are actually drained by the rebalancer.
	fleetOrchestrator := buildFleetOrchestrator(cfg, metadataDB)

	// Console API: separate HTTP surface for the tenant console
	// (react frontend in frontend/). It runs on its own listener
	// so a saturated S3 data plane cannot starve the management
	// controls operators use to diagnose it. The default address
	// is :8081 when the operator has not overridden it in config.
	consoleSrv := startConsoleAPI(cfg, metadataDB, tenantStore, authStore, authHooks, billingSink, billingProvider, fleetOrchestrator)

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
	if err := startListener(srv, cfg.Gateway.TLS, cfg.Env, "gateway"); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("gateway: listen: %v", err)
	}
	<-workerDone
	if rebalancerDone != nil {
		<-rebalancerDone
	}
	if orphanGCDone != nil {
		<-orphanGCDone
	}
	if crossCellDone != nil {
		<-crossCellDone
	}
	if repairDone != nil {
		<-repairDone
	}
}

// tlsVersionLabel renders a crypto/tls version constant as the
// "1.2" / "1.3" string the log line advertises. Centralised so the
// log display stays correct when config.TLSConfig adds support
// for a new TLS version: extending MinTLSVersion's switch is then
// paired with one extra case here, instead of leaving the log
// line silently misreporting the previous default. An unknown
// constant renders as "0x%04x" rather than misleadingly defaulting
// to the lowest supported version.
func tlsVersionLabel(v uint16) string {
	switch v {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return fmt.Sprintf("0x%04x", v)
	}
}

// warnIfSlowlorisDisabled emits a structured WARN log line at
// startup when the named listener is enabled (ListenAddr non-
// empty) but its ReadHeaderTimeout is zero or negative. Zero
// ReadHeaderTimeout is a legitimate operator opt-out (the field
// docs document this and Validate() accepts it), but it disables
// the cheaper header-stall Slowloris defence entirely, so the
// fleet observability signal is what operators rely on to notice
// the misconfiguration. Logging at startup rather than refusing
// to start preserves the opt-out while making it auditable: a
// log scraper match on "slowloris-defence-disabled" surfaces
// every node running with the protection off. (Devin Review
// ANALYSIS_0003 on PR #80 flagged the silent-opt-out path.)
func warnIfSlowlorisDisabled(name, listenAddr string, readHeader config.Duration) {
	if listenAddr == "" {
		return
	}
	if readHeader > 0 {
		return
	}
	log.Printf(
		"gateway: %s listener WARN slowloris-defence-disabled: read_header_timeout=%s on %s; Slowloris header-stall protection is OFF (zero ReadHeaderTimeout means the entire ReadTimeout window must elapse before a slow-header attacker is dropped). Set %s.read_header_timeout to a non-zero duration (default 10s) unless you have a deliberate reason to disable.",
		name, readHeader.ToDuration(), listenAddr, name,
	)
}

// logEffectiveMaxHeaderBytes records the effective MaxHeaderBytes
// ceiling at startup for the named listener. Sibling of
// warnIfSlowlorisDisabled — both surface security-critical effective
// configuration in the startup log so operators auditing a node have
// a single regex (`gateway: %s listener …`) that captures the whole
// surface.
//
// PR #80 lowered the default effective ceiling from Go stdlib's
// 1 MiB to 64 KiB (DefaultMaxHeaderBytes in internal/config) as a
// header-size Slowloris defence — an attacker mass-spraying enormous
// header blobs is rejected with HTTP 431 instead of being allowed to
// allocate a megabyte per connection. The reduction is intentional
// (Devin Review ANALYSIS_0003 on PR #80 flagged the upgrade hazard
// for deployments running heavy JWT auth or many `x-amz-meta-*`
// custom metadata headers), and the structured startup log line
// here is the operator-facing audit trail: a log-scraper grep on
// `effective_max_header_bytes=` surfaces the deployed value across
// every node so a deployment hitting unexpected 431s can verify the
// ceiling without re-reading config. Operators can raise it per
// listener via `<name>.max_header_bytes`.
func logEffectiveMaxHeaderBytes(name, listenAddr string, maxBytes int) {
	if listenAddr == "" {
		return
	}
	log.Printf(
		"gateway: %s listener effective_max_header_bytes=%d on %s (raise %s.max_header_bytes if your deployment uses unusually large request headers, e.g. heavy JWT auth or many x-amz-meta-* custom metadata keys; pre-PR-#80 Go stdlib default was 1048576)",
		name, maxBytes, listenAddr, name,
	)
}

// startListener picks ListenAndServeTLS vs ListenAndServe based on
// the supplied TLS config. It first runs t.Validate so a partial
// config (exactly one of cert_path or key_path set) surfaces as a
// startup error instead of silently downgrading to plain HTTP —
// the wrong failure mode for a production deployment that
// configured TLS but typoed one of the paths.
//
// When TLS is enabled it parses the MinVersion from cfg and
// applies it to srv.TLSConfig before handing the cert / key paths
// to ListenAndServeTLS. When TLS is disabled and env ==
// "production" it logs a WARNING so operators terminating TLS at
// an upstream load balancer can ignore the signal while
// deployments serving clients directly notice it.
//
// name labels the listener in log lines ("gateway", "console",
// "health") so operators with multiple listeners on the same
// process can tell them apart. Both the TLS-enabled and the
// production-warning log lines share the "gateway: %s …" prefix
// (where %s is the per-listener name) so log scrapers can match
// every gateway-process line with one regex.
func startListener(srv *http.Server, t config.TLSConfig, env, name string) error {
	if err := t.Validate(name); err != nil {
		return err
	}
	if t.Enabled() {
		gotls, err := t.BuildGoTLSConfig()
		if err != nil {
			return fmt.Errorf("%s: build tls config: %w", name, err)
		}
		srv.TLSConfig = gotls
		log.Printf("gateway: %s TLS enabled (cert=%s min_version=%s)", name, t.CertPath, tlsVersionLabel(gotls.MinVersion))
		return srv.ListenAndServeTLS(t.CertPath, t.KeyPath)
	}
	if env == "production" {
		log.Printf("gateway: %s WARNING running without TLS in production mode (%s.tls.cert_path / key_path are empty); only safe when an upstream load balancer terminates TLS", name, name)
	}
	return srv.ListenAndServe()
}

// errProductionAuthRequired is returned by checkProductionAuth
// when the gateway is started with env=production but no tenant
// bindings of any kind (no Postgres DSN AND no --tenants file).
// Exposed as a sentinel so cmd/gateway can log a friendly
// message and main_test.go can errors.Is against it.
var errProductionAuthRequired = errors.New("gateway: env=production but no tenant bindings are configured (no control_plane.metadata_dsn AND no --tenants file with bindings); refusing to start with an effectively-unauthenticated handler")

// checkProductionAuth refuses to start the gateway when
// cfg.Env == "production" and the tenant store is the dev /
// scaffolding configuration (in-memory store with zero bindings).
// The intent is to make it impossible to ship a production
// deployment whose handler effectively serves every request under
// the AnonymousTenant fallback.
//
// metadataDB != nil means a Postgres-backed tenant store is wired —
// even a fresh deploy with zero bindings is fine because new tenant
// rows are added by the console signup flow. metadataDB == nil
// with a non-empty store means the operator loaded bindings via
// --tenants, which is also a supported production config (static
// HMAC keys). The error path only triggers when both are missing:
// no Postgres connection AND no static bindings.
//
// This guard is layered alongside the s3compat handler's
// RequireAuth check: even if the operator zeroes out Auth on the
// handler config, every request returns 500 instead of writing
// data under AnonymousTenant.
//
// The function returns an error rather than calling log.Fatalf so
// tests can exercise both branches in-process (no subprocess
// re-exec gymnastics) and the production-startup wrapper
// enforceProductionAuth below handles the fatal transition.
func checkProductionAuth(env string, metadataDB *sql.DB, tenantStore auth.TenantStore) error {
	if env != "production" {
		return nil
	}
	if metadataDB != nil {
		return nil
	}
	if tenantStore != nil && tenantStore.Size() > 0 {
		return nil
	}
	return errProductionAuthRequired
}

// enforceProductionAuth wraps checkProductionAuth at the startup
// callsite: a non-nil error is fatal (the gateway must not boot
// without a tenant store in production). Tests should call
// checkProductionAuth directly so they can errors.Is against the
// sentinel without forking the test binary.
func enforceProductionAuth(env string, metadataDB *sql.DB, tenantStore auth.TenantStore) {
	if err := checkProductionAuth(env, metadataDB, tenantStore); err != nil {
		log.Fatalf("%s", err)
	}
}

// errProductionManifestEncryptionRequired is returned by
// checkProductionManifestEncryption when the gateway is started
// with env=production, a persistent manifest store is selected,
// and the BodyEncryptor for that store has not been configured
// (manifest_body_key_path is empty). Exposed as a sentinel so
// cmd/gateway can log a friendly message and main_test.go can
// errors.Is against it.
var errProductionManifestEncryptionRequired = errors.New("gateway: env=production with a persistent manifest store (Postgres or embedded SQLite) but no manifest_body_key_path is configured; manifest JSON will be stored as plaintext. Set encryption.manifest_body_key_path or use env=development")

// checkProductionManifestEncryption refuses to start the gateway
// when cfg.Env == "production" and a persistent manifest store
// would persist manifest bodies unencrypted. Without a
// BodyEncryptor key the manifest table leaks object keys, piece
// locators, sizes, and the wrapped DEK to anyone with read access
// to the store — the exact threat model the Phase 2 manifest body
// encryption was added to defend against (see
// manifest_store.BodyEncryptor and docs/PROPOSAL.md §3.7). Both
// persistent backends are covered: Postgres (plaintext JSONB) and
// the embedded SQLite store (plaintext blob on disk).
//
// The check is conditional on a persistent manifest store being
// active: the in-memory dev store has no persistent backing
// table, so a manifest body key would be ineffectual there. This
// matches the threat model precisely — the guard fires when the
// gateway is about to write tenant data to a long-lived store
// that survives the process lifetime, never when the process is
// the only authority on the manifests.
//
// Returns a sentinel error rather than calling log.Fatalf so
// tests can exercise both branches without subprocess re-exec
// gymnastics; the production-startup wrapper
// enforceProductionManifestEncryption handles the fatal
// transition.
func checkProductionManifestEncryption(env string, manifestStorePersistent bool, manifestBodyKeyPath string) error {
	if env != "production" {
		return nil
	}
	if !manifestStorePersistent {
		return nil
	}
	if manifestBodyKeyPath != "" {
		return nil
	}
	return errProductionManifestEncryptionRequired
}

// enforceProductionManifestEncryption wraps
// checkProductionManifestEncryption at the startup callsite. A
// non-nil error is fatal (the gateway must not boot in production
// without manifest body encryption, otherwise tenant manifests
// are stored as plaintext in the persistent store — Postgres JSONB
// or the embedded SQLite file). Tests should call
// checkProductionManifestEncryption directly so they can errors.Is
// against the sentinel.
func enforceProductionManifestEncryption(env string, manifestStorePersistent bool, manifestBodyKeyPath string) {
	if err := checkProductionManifestEncryption(env, manifestStorePersistent, manifestBodyKeyPath); err != nil {
		log.Fatalf("SECURITY: env=production with a persistent manifest store (Postgres or embedded SQLite) but no manifest_body_key_path is configured; manifest JSON will be stored as plaintext. Set encryption.manifest_body_key_path or use env=development.")
	}
}

// errProductionTokenStoreRequired is returned by
// checkProductionTokenStore when the gateway is started with
// env=production but no console.jwt_signing_key_path is configured,
// so the console would fall back to the process-local
// MemoryTokenStore. Exposed as a sentinel so cmd/gateway can log a
// friendly message and main_test.go can errors.Is against it.
var errProductionTokenStoreRequired = errors.New("gateway: env=production but no console.jwt_signing_key_path is configured; the console would fall back to the in-memory MemoryTokenStore, whose tokens are lost on restart and are not shared across replicas behind a load balancer. Set console.jwt_signing_key_path to a PEM-encoded RSA private key or use env=development")

// checkProductionTokenStore refuses to start the gateway when
// cfg.Env == "production", the console API is enabled, and no JWT
// signing key is configured. The MemoryTokenStore the console
// otherwise falls back to is process-local: every issued session
// token is dropped on restart, and two gateway replicas behind a load
// balancer mint tokens the other cannot validate, so a multi-replica
// production deploy would log users out on every rolling restart and
// fail authentication for any request that lands on a replica other
// than the one that issued the token. The stateless JWTTokenStore
// (signed, self-contained, validated against a shared public key) is
// the only production-safe option, so production must configure a
// signing key.
//
// The guard is gated on consoleListenAddr because the console API is
// opt-in: when it is empty startConsoleAPI returns before
// buildTokenStore is ever called, so no TokenStore exists to be
// unsafe. An S3 data-plane-only production gateway (console disabled)
// must not be forced to configure a signing key it never uses.
//
// Layered alongside enforceProductionAuth: that guard protects the
// S3 data-plane authenticator; this one protects the console session
// layer. Returns a sentinel error rather than calling log.Fatalf so
// tests can exercise both branches in-process.
func checkProductionTokenStore(env, consoleListenAddr, jwtSigningKeyPath string) error {
	if env != "production" {
		return nil
	}
	if consoleListenAddr == "" {
		return nil
	}
	if jwtSigningKeyPath != "" {
		return nil
	}
	return errProductionTokenStoreRequired
}

// enforceProductionTokenStore wraps checkProductionTokenStore at the
// startup callsite: a non-nil error is fatal. Tests should call
// checkProductionTokenStore directly so they can errors.Is against
// the sentinel without forking the test binary.
func enforceProductionTokenStore(env, consoleListenAddr, jwtSigningKeyPath string) {
	if err := checkProductionTokenStore(env, consoleListenAddr, jwtSigningKeyPath); err != nil {
		log.Fatalf("%s", err)
	}
}

// buildTokenStore selects the console TokenStore from config. When
// cfg.Console.JWTSigningKeyPath is set it loads the RSA key and
// returns a stateless JWTTokenStore; otherwise it returns the
// process-local MemoryTokenStore with a loud warning. The "iss"
// claim is taken from cfg.ControlPlane.AuthIssuer, falling back to a
// stable default when the operator left it unset, so a dev who wires
// only a key still gets verifiable tokens.
func buildTokenStore(cfg config.Config) console.TokenStore {
	p := cfg.Console.JWTSigningKeyPath
	if p == "" {
		// MemoryTokenStore is process-local and loses every
		// issued token on restart; production never reaches here
		// because enforceProductionTokenStore fails closed first.
		log.Printf("console: jwt_signing_key_path not set; using in-memory MemoryTokenStore — DO NOT use in production (tokens are lost on restart and not shared across replicas)")
		return console.NewMemoryTokenStore()
	}
	key, err := console.LoadRSAPrivateKeyPEM(p)
	if err != nil {
		log.Fatalf("gateway: load console JWT signing key: %v", err)
	}
	issuer := cfg.ControlPlane.AuthIssuer
	if issuer == "" {
		issuer = defaultJWTIssuer
	}
	store, err := console.NewJWTTokenStore(console.JWTConfig{
		SigningKey: key,
		Issuer:     issuer,
		TTL:        time.Duration(cfg.Console.JWTTokenTTL),
		KeyID:      jwtKeyIDFromKey(key),
	})
	if err != nil {
		log.Fatalf("gateway: build console JWT token store: %v", err)
	}
	log.Printf("console: stateless JWT session tokens enabled (key=%s issuer=%s ttl=%s)", p, issuer, store.TokenTTL())
	return store
}

// defaultJWTIssuer is the "iss" claim used when the operator wires a
// JWT signing key but leaves control_plane.auth_issuer empty.
const defaultJWTIssuer = "zk-object-fabric-console"

// jwtKeyIDFromKey derives a short, stable key identifier from the
// public modulus so issued tokens carry a "kid" that pins them to a
// specific key. This is purely forward-looking: it lets a future
// multi-key rotation select the verifier by kid without re-minting
// outstanding tokens. The value is a non-secret fingerprint of the
// public key (which is published anyway).
func jwtKeyIDFromKey(key *rsa.PrivateKey) string {
	sum := sha256.Sum256(key.PublicKey.N.Bytes())
	return hex.EncodeToString(sum[:8])
}

// checkAllTLSConfigs validates the TLS config for every listener
// the gateway can start (gateway data-plane, console, health) and
// returns the first non-nil validation error. The returned error
// names the offending listener so the operator can immediately
// see which block is misconfigured. Tests call this directly so
// they can assert per-listener error wrapping without forking
// the test binary; main calls validateAllTLSConfigs which fatals.
func checkAllTLSConfigs(cfg config.Config) error {
	if err := cfg.Gateway.TLS.Validate("gateway"); err != nil {
		return err
	}
	if cfg.Console.ListenAddr != "" {
		if err := cfg.Console.TLS.Validate("console"); err != nil {
			return err
		}
	}
	if cfg.Health.ListenAddr != "" {
		if err := cfg.Health.TLS.Validate("health"); err != nil {
			return err
		}
	}
	return nil
}

// validateAllTLSConfigs is the startup-time wrapper: any
// validation failure on any listener's TLS block is a fatal
// startup error, full stop. The console and health listeners
// run startListener inside goroutines that only log.Printf on
// error, which would otherwise leave a partial TLS config as a
// silent soft failure — exactly the wrong failure mode for a
// production deploy that intended TLS but typoed a path. Doing
// the validation here means the process refuses to boot rather
// than running with a listener mysteriously absent.
//
// startListener also runs t.Validate internally so a future call
// site that forgets to pre-validate still surfaces the error;
// the pre-flight check is the architectural correctness fix and
// the in-listener check is defence-in-depth.
func validateAllTLSConfigs(cfg config.Config) {
	if err := checkAllTLSConfigs(cfg); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}

// errProductionLocalCMK is returned by checkProductionLocalCMK
// when the gateway is started with env=production and the local
// file CMK wrapper is in use without the explicit AllowLocalCMK
// escape hatch. A plaintext CMK on the gateway disk trivially
// defeats the encryption envelope (anyone with disk access can
// unwrap every tenant DEK), so the default behaviour is
// fail-closed. Exposed as a sentinel so tests can errors.Is
// against it.
var errProductionLocalCMK = errors.New("gateway: env=production but the local file CMK wrapper is in use; this exposes every tenant DEK to anyone with gateway disk access. Use AWS KMS (kms://) or HashiCorp Vault Transit (vault://). For HSM-fuse deployments where the local file path maps to hardware-backed key material, set --allow-local-cmk or encryption.allow_local_cmk=true to override this guard")

// checkProductionLocalCMK returns an error when the gateway is
// started with env=production using the local-file CMK wrapper
// and AllowLocalCMK is not set. In every other configuration
// (non-production, AWS KMS, Vault transit, or local-file with
// an explicit operator opt-in) the function returns nil.
//
// Returns a sentinel error rather than calling log.Fatalf so
// tests can exercise both branches without subprocess re-exec
// gymnastics; the production-startup wrapper
// enforceProductionLocalCMK handles the fatal transition.
func checkProductionLocalCMK(env string, allowLocalCMK bool, cmkURI, holderClass string) error {
	if env != "production" {
		return nil
	}
	if !isLocalFileCMK(cmkURI, holderClass) {
		return nil
	}
	if allowLocalCMK {
		return nil
	}
	return errProductionLocalCMK
}

// warnProductionLocalCMK emits a critical warning when the
// gateway is started in production with the local-file CMK
// wrapper AND the operator has set AllowLocalCMK to opt out of
// the fail-closed default. The log message exists so the misconfig
// shows up loudly in startup logs even when the override is
// intentional — an operator who flips the flag on for an HSM-fuse
// deployment still wants the gateway to remind them why they did
// it during every restart.
//
// When env != production OR the CMK is not the local-file wrapper
// the function is a no-op.
func warnProductionLocalCMK(env string, cmkURI, holderClass string) {
	if env != "production" {
		return
	}
	if !isLocalFileCMK(cmkURI, holderClass) {
		return
	}
	log.Printf("SECURITY: using local file CMK in production (uri=%s holder=%s) under --allow-local-cmk / encryption.allow_local_cmk override; this is only safe if the path maps to hardware-backed key material (HSM-fuse, TPM). A plaintext CMK file on the gateway disk defeats the encryption envelope.", cmkURI, holderClass)
}

// enforceProductionLocalCMK wraps checkProductionLocalCMK at the
// startup callsite: a non-nil error is fatal (the gateway must
// not boot in production with a plaintext local-file CMK unless
// the operator has explicitly opted in). When the operator has
// flipped AllowLocalCMK on, the function downgrades to a logged
// warning via warnProductionLocalCMK so the misconfig still
// surfaces in startup logs for HSM-fuse deployments where the
// override is intentional.
func enforceProductionLocalCMK(env string, allowLocalCMK bool, cmkURI, holderClass string) {
	if err := checkProductionLocalCMK(env, allowLocalCMK, cmkURI, holderClass); err != nil {
		log.Fatalf("%s", err)
	}
	// Only emit the audit warning when the operator has flipped
	// the AllowLocalCMK escape hatch AND the configuration would
	// otherwise have been rejected (production + local-file CMK).
	// The pre-fix code called warnProductionLocalCMK
	// unconditionally, which was a no-op for every other
	// combination but obscured the intent: warning belongs ONLY
	// to the explicit override case so operators reading startup
	// logs in non-production / KMS / Vault configurations are not
	// confused by a security warning that does not apply to them.
	if env == "production" && isLocalFileCMK(cmkURI, holderClass) && allowLocalCMK {
		warnProductionLocalCMK(env, cmkURI, holderClass)
	}
}

// isLocalFileCMK reports whether the resolved wrapper is the
// LocalFileWrapper. Detection works off the (uri, holderClass)
// pair selectGatewayWrapper records so we do not have to type-
// assert against the concrete client_sdk.LocalFileWrapper type
// from main.
func isLocalFileCMK(cmkURI, holderClass string) bool {
	if holderClass == "gateway_hsm" && (cmkURI == "" || strings.HasPrefix(cmkURI, "cmk://local")) {
		return true
	}
	return false
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

// buildFleetOrchestrator constructs the FleetOrchestrator used
// for distributed migration-job coordination. The JobStore is
// PgJobStore-backed when a metadata DB is available so two
// gateways sharing the same Postgres serialise their claims
// through the migration_jobs table; otherwise an
// InMemoryJobStore keeps single-node and dev deployments
// working without a DB dependency.
//
// NodeID resolution: explicit cfg.Rebalancer.NodeID wins;
// otherwise os.Hostname is used (the typical pod or VM
// identifier). A last-resort fallback to "gateway-unknown" only
// fires when Hostname errors, which is rare enough that we log
// loudly so an operator can see it.
//
// The orchestrator's per-cell CellLimits are sourced from the
// Rebalancer.Targets list — every target's destination cell
// gets a default cap of 1 unless the operator overrides it
// elsewhere. The cap is a global guarantee across the fleet
// because ListActiveJobs is the source of truth.
//
// The returned orchestrator's RunOnce ticker is NOT started
// here; PRs in this hardening series add the enqueue path
// (and a dispatch loop) when the production scheduler is
// finalised. For now the orchestrator exists so the
// management console's /api/v1/migrations endpoints have a
// real store to introspect.
func buildFleetOrchestrator(cfg config.Config, metadataDB *sql.DB) *migration.FleetOrchestrator {
	store := buildJobStore(metadataDB)
	nodeID := resolveRebalancerNodeID(cfg.Rebalancer)
	ttl := time.Duration(cfg.Rebalancer.ClaimTTL)
	limits := make([]migration.CellLimits, 0, len(cfg.Rebalancer.Targets))
	seen := map[string]bool{}
	for _, t := range cfg.Rebalancer.Targets {
		cell := t.PrimaryBackend
		if cell == "" || seen[cell] {
			continue
		}
		seen[cell] = true
		// Default to 1 because the orchestrator's per-cell
		// concurrency cap is the only thing preventing a hot
		// dest from being overrun by the rebalancer. Operators
		// who want higher fan-out can override the cap in a
		// follow-up config field.
		limits = append(limits, migration.CellLimits{CellID: cell, MaxConcurrentJobs: 1})
	}
	o, err := migration.NewFleetOrchestratorWithStore(migration.FleetOrchestratorConfig{
		Store:    store,
		NodeID:   nodeID,
		Limits:   limits,
		ClaimTTL: ttl,
		Logger:   log.New(os.Stdout, "orchestrator ", log.LstdFlags),
	})
	if err != nil {
		// Construction can only fail when Store or NodeID
		// are missing; both are populated above. Fatal here
		// rather than silently return nil so a regression
		// surfaces at startup.
		log.Fatalf("gateway: build fleet orchestrator: %v", err)
	}
	log.Printf("gateway: fleet orchestrator node_id=%s store=%s", nodeID, jobStoreLabel(store))
	return o
}

// buildJobStore returns a Postgres-backed JobStore when the
// metadata DB is configured, falling back to InMemoryJobStore
// for single-node / dev deployments. The fall-back is logged
// loudly because an in-memory store loses every claim on
// gateway restart — fine for dev but not for any deployment
// where the rebalancer is expected to make sustained progress.
func buildJobStore(metadataDB *sql.DB) migration.JobStore {
	if metadataDB == nil {
		return migration.NewInMemoryJobStore(nil)
	}
	store, err := migration.NewPgJobStore(migration.PgConfig{DB: metadataDB})
	if err != nil {
		log.Printf("gateway: build PgJobStore: %v; falling back to in-memory", err)
		return migration.NewInMemoryJobStore(nil)
	}
	return store
}

// jobStoreLabel returns a short type tag used in the startup
// log line so operators can confirm which JobStore the gateway
// resolved without reading the source.
func jobStoreLabel(s migration.JobStore) string {
	switch s.(type) {
	case *migration.PgJobStore:
		return "postgres"
	case *migration.InMemoryJobStore:
		return "in-memory"
	default:
		return "unknown"
	}
}

// resolveRebalancerNodeID picks the identifier the JobStore
// uses to attribute claim ownership. Explicit cfg wins;
// otherwise os.Hostname is the usual container / pod name;
// a fallback of "gateway-unknown" only fires when Hostname
// errors (rare in practice but defended against because the
// orchestrator constructor refuses an empty NodeID).
func resolveRebalancerNodeID(cfg config.RebalancerConfig) string {
	if cfg.NodeID != "" {
		return cfg.NodeID
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		log.Printf("gateway: os.Hostname() failed (%v); falling back to gateway-unknown — set rebalancer.node_id explicitly", err)
		return "gateway-unknown"
	}
	return host
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

// openEmbeddedDB opens the local SQLite database that backs the
// embedded / single-node deployment profile. It returns (nil, nil)
// unless a metadata DSN is absent AND an embedded path is set, so
// callers can branch on embeddedDB == nil exactly as they do for the
// Postgres pool. Postgres always wins: when MetadataDSN is set the
// embedded path is ignored and this returns nil so the gateway does
// not open a redundant, never-read SQLite file.
func openEmbeddedDB(cfg config.Config) (*sql.DB, error) {
	if cfg.ControlPlane.MetadataDSN != "" {
		return nil, nil
	}
	if cfg.ControlPlane.EmbeddedDBPath == "" {
		return nil, nil
	}
	db, err := embeddeddb.Open(cfg.ControlPlane.EmbeddedDBPath)
	if err != nil {
		return nil, fmt.Errorf("open embedded metadata DB: %w", err)
	}
	log.Printf("gateway: embedded SQLite metadata store enabled (path=%s); manifest, content_index, auth, and billing persist locally without Postgres", cfg.ControlPlane.EmbeddedDBPath)
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
func buildContentIndex(cfg config.Config, db, embeddedDB *sql.DB) content_index.Store {
	if !cfg.Dedup.Enabled {
		log.Printf("gateway: dedup disabled (dedup.enabled = false); content_index store will not be built")
		return nil
	}
	if db == nil {
		if embeddedDB != nil {
			store, err := cisqlite.New(cisqlite.Config{DB: embeddedDB})
			if err != nil {
				log.Fatalf("gateway: build embedded content_index store: %v", err)
			}
			log.Printf("gateway: embedded SQLite content_index store enabled (default_scope=%s default_level=%s)", cfg.Dedup.DefaultScope, cfg.Dedup.DefaultLevel)
			return store
		}
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

func buildManifestStore(cfg config.Config, db, embeddedDB *sql.DB) manifest_store.ManifestStore {
	bodyEnc := buildManifestBodyEncryptor(cfg)
	if db == nil {
		if embeddedDB != nil {
			store, err := sqlitestore.New(sqlitestore.Config{DB: embeddedDB, BodyEncryptor: bodyEnc})
			if err != nil {
				log.Fatalf("gateway: build embedded manifest store: %v", err)
			}
			log.Printf("gateway: embedded SQLite manifest store enabled")
			return store
		}
		log.Printf("gateway: no control_plane.metadata_dsn; using in-memory manifest store (dev only)")
		return memory.New()
	}
	store, err := pgstore.New(pgstore.Config{DB: db, BodyEncryptor: bodyEnc})
	if err != nil {
		log.Fatalf("gateway: build postgres manifest store: %v", err)
	}
	return store
}

// buildManifestBodyEncryptor loads the optional manifest-body AEAD
// encryptor from cfg.Encryption.ManifestBodyKeyPath. It returns nil
// when no key is configured (bodies stored as plaintext JSON). The
// same encryptor is shared by the Postgres and embedded SQLite
// manifest stores, so a deployment can switch backends without
// re-keying.
func buildManifestBodyEncryptor(cfg config.Config) manifest_store.BodyEncryptor {
	p := cfg.Encryption.ManifestBodyKeyPath
	if p == "" {
		return nil
	}
	key, rerr := os.ReadFile(p)
	if rerr != nil {
		log.Fatalf("gateway: read manifest body key %q: %v", p, rerr)
	}
	enc, eerr := manifest_store.NewAEADBodyEncryptor(key)
	if eerr != nil {
		log.Fatalf("gateway: build manifest body encryptor: %v", eerr)
	}
	log.Printf("gateway: manifest body encryption enabled (key=%s)", p)
	return enc
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

// buildMultipartStore returns the multipart-session Store the S3
// handler consumes. When metadata_dsn is configured the gateway
// uses the Postgres-backed store so in-flight uploads survive
// restart and let a client complete an upload against a different
// node than the one that handled CreateMultipartUpload. Without a
// metadata DSN the gateway falls back to the in-memory store
// (intended for `go run` / dev use only).
//
// The expiry sweeper's cleanup callback fans DeletePiece out to
// every part's backend so abandoned ciphertext does not accumulate
// when a client never calls CompleteMultipartUpload or
// AbortMultipartUpload. A nil registry entry for a part's backend
// is logged but does not stop the sweep — the alternative would
// be the sweeper getting stuck on a single ill-formed upload.
//
// The returned closer must be called at gateway shutdown to stop
// the sweeper goroutine; it is nil for the in-memory store.
func buildMultipartStore(
	db *sql.DB,
	enc *s3compat.GatewayEncryption,
	registry map[string]providers.StorageProvider,
) (multipart.Store, func()) {
	if db == nil {
		log.Printf("gateway: no control_plane.metadata_dsn; using in-memory multipart store (dev only — in-flight uploads do NOT survive restart)")
		return multipart.NewMemoryStore(), nil
	}
	pgCfg := multipart.PostgresConfig{
		DB:     db,
		Logger: log.New(os.Stdout, "multipart_pg ", log.LstdFlags),
		Cleanup: func(ctx context.Context, _ *multipart.Upload, parts []multipart.Part) {
			for _, p := range parts {
				provider, ok := registry[p.Backend]
				if !ok {
					log.Printf("gateway: multipart expiry: backend %q not registered; skipping piece %s", p.Backend, p.PieceID)
					continue
				}
				if err := provider.DeletePiece(ctx, p.PieceID); err != nil {
					log.Printf("gateway: multipart expiry: delete piece %s on %s: %v", p.PieceID, p.Backend, err)
				}
			}
		},
	}
	if enc != nil {
		pgCfg.Wrapper = enc.Wrapper
		pgCfg.CMK = enc.CMK
	}
	store, err := multipart.NewPostgresStore(pgCfg)
	if err != nil {
		log.Fatalf("gateway: build postgres multipart store: %v", err)
	}
	log.Printf("gateway: postgres multipart store enabled (upload_ttl=%s sweep_interval=%s)",
		multipart.DefaultUploadTTL, multipart.DefaultExpirySweepInterval)
	return store, func() { _ = store.Close() }
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
// configured, the embedded SQLite sink when the embedded profile is
// active (so usage events persist locally and feed the console usage
// page without ClickHouse), and otherwise the development LoggerSink.
// The returned value satisfies api/s3compat.BillingSink; the
// SQLite sink additionally implements Close(ctx) (drained on
// shutdown) and console.UsageQuery.
func buildBillingSink(cfg config.Config, embeddedDB *sql.DB) interface {
	Emit(event billing.UsageEvent)
} {
	if cfg.Billing.ClickHouseURL == "" {
		if embeddedDB != nil {
			sink, err := billing.NewSQLiteSink(billing.SQLiteSinkConfig{
				DB:            embeddedDB,
				BatchSize:     cfg.Billing.BatchSize,
				FlushInterval: cfg.Billing.FlushInterval.ToDuration(),
				Logger:        log.New(os.Stdout, "billing ", log.LstdFlags),
			})
			if err != nil {
				log.Fatalf("gateway: build embedded billing sink: %v", err)
			}
			log.Printf("gateway: embedded SQLite billing sink enabled")
			return sink
		}
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
func startHealthMonitor(ctx context.Context, hc config.HealthConfig, cache hot_object_cache.HotObjectCache, env string) *health.Monitor {
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
		// Slowloris hardening for the health endpoints: the
		// internal /health surface is normally fronted by a
		// cluster-internal listener (the comment block on
		// HealthConfig.ListenAddr notes "e.g. :29090"), but a
		// misconfigured NetworkPolicy or a debug deployment that
		// exposes the listener directly should not be silently
		// vulnerable. Apply the same five knobs the gateway and
		// console servers use (ReadTimeout, WriteTimeout,
		// ReadHeaderTimeout, IdleTimeout, MaxHeaderBytes); the
		// defaults (ReadTimeout 30s, WriteTimeout 30s,
		// ReadHeaderTimeout 10s, IdleTimeout 120s, MaxHeaderBytes
		// 64 KiB) come from HealthConfig.Default() so an upgrade
		// picks them up automatically without a config edit.
		//
		// ReadTimeout and WriteTimeout are included even though
		// the current /health surface is GET-only with tiny
		// responses: they are defence-in-depth for any future
		// POST/PUT health endpoint (drain control, manual quorum
		// override) and ReadTimeout is also the upper bound
		// validateTimeoutOrder enforces ReadHeaderTimeout to
		// stay strictly below — a missing ReadTimeout would
		// silently let an operator set ReadHeaderTimeout to any
		// value, masking the misconfig the validator is meant
		// to catch on the gateway and console. (Devin Review
		// ANALYSIS_0004 on PR #80 added ReadTimeout;
		// ANALYSIS_0001 on ef092a6 added WriteTimeout for the
		// same symmetry.)
		warnIfSlowlorisDisabled("health", hc.ListenAddr, hc.ReadHeaderTimeout)
		healthMaxHeaderBytes := config.EffectiveMaxHeaderBytes(hc.MaxHeaderBytes)
		logEffectiveMaxHeaderBytes("health", hc.ListenAddr, healthMaxHeaderBytes)
		srv := &http.Server{
			Addr:              hc.ListenAddr,
			Handler:           mon.ServeMux(""),
			ReadTimeout:       hc.ReadTimeout.ToDuration(),
			WriteTimeout:      hc.WriteTimeout.ToDuration(),
			ReadHeaderTimeout: hc.ReadHeaderTimeout.ToDuration(),
			IdleTimeout:       hc.IdleTimeout.ToDuration(),
			MaxHeaderBytes:    healthMaxHeaderBytes,
		}
		go func() {
			log.Printf("gateway: health endpoints on %s", hc.ListenAddr)
			if err := startListener(srv, hc.TLS, env, "health"); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	orchestrator *migration.FleetOrchestrator,
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
	tokens := buildTokenStore(cfg)

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
		Orchestrator:    orchestrator,
	})
	mux := http.NewServeMux()
	h.Register(mux)

	// Slowloris hardening: ReadHeaderTimeout / IdleTimeout /
	// MaxHeaderBytes mirror the gateway's posture so a
	// misconfigured ingress that accidentally exposes the console
	// API to the internet is not silently exploitable. The
	// defaults are set in config.Default(); MaxHeaderBytes routes
	// through config.EffectiveMaxHeaderBytes so an explicit-0 in
	// the JSON config re-floors to config.DefaultMaxHeaderBytes
	// (64 KiB) instead of falling through to Go's 1 MiB stdlib
	// default (matches gateway and health; Devin Review
	// ANALYSIS_0001 on PR #80).
	warnIfSlowlorisDisabled("console", cfg.Console.ListenAddr, cfg.Console.ReadHeaderTimeout)
	consoleMaxHeaderBytes := config.EffectiveMaxHeaderBytes(cfg.Console.MaxHeaderBytes)
	logEffectiveMaxHeaderBytes("console", cfg.Console.ListenAddr, consoleMaxHeaderBytes)
	srv := &http.Server{
		Addr:              cfg.Console.ListenAddr,
		Handler:           mux,
		ReadTimeout:       cfg.Console.ReadTimeout.ToDuration(),
		WriteTimeout:      cfg.Console.WriteTimeout.ToDuration(),
		ReadHeaderTimeout: cfg.Console.ReadHeaderTimeout.ToDuration(),
		IdleTimeout:       cfg.Console.IdleTimeout.ToDuration(),
		MaxHeaderBytes:    consoleMaxHeaderBytes,
	}
	go func() {
		log.Printf("gateway: console API on %s", cfg.Console.ListenAddr)
		if err := startListener(srv, cfg.Console.TLS, cfg.Env, "console"); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("gateway: console listener: %v", err)
		}
	}()
	return srv
}

// buildAuthStore returns the Postgres-backed AuthStore when a
// metadata DSN is configured, the embedded SQLite store when the
// embedded profile is active, and MemoryAuthStore otherwise (dev
// mode). The store is shared between the console signup / login
// handler and the S3 handler's email-verification gate.
func buildAuthStore(db, embeddedDB *sql.DB) console.AuthStore {
	if db == nil {
		if embeddedDB != nil {
			store, err := console.NewSQLiteAuthStore(embeddedDB)
			if err != nil {
				log.Fatalf("gateway: build embedded auth store: %v", err)
			}
			log.Printf("gateway: embedded SQLite auth store enabled")
			return store
		}
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

// buildTracer constructs the request tracer from config. When
// disabled or no exporter is wired, it returns a tracer with the
// no-op exporter so calling code does not need to nil-check.
func buildTracer(cfg config.TracingConfig) *tracing.Tracer {
	name := cfg.ServiceName
	if name == "" {
		name = "zk-object-fabric"
	}
	// The OTLP / Jaeger exporter is intentionally not wired here:
	// the tracing package keeps the SDK out of the core build to
	// keep dependency footprint small. Operators wiring a real
	// backend can add an Exporter implementation in a build tag.
	return tracing.New(name, tracing.NoopExporter{})
}

// integrityFailureSink adapts metrics.Registry to the
// s3compat.IntegrityFailureSink interface so the HTTP handler
// can emit zkof_integrity_failure_total /
// zkof_integrity_claim_unrecognized_total without importing
// internal/metrics. A zero-value sink (nil registry) is a no-op
// — useful for tests that wire the handler without a registry.
type integrityFailureSink struct {
	r *metrics.Registry
}

func (s integrityFailureSink) Inc(backend string) {
	if s.r == nil {
		return
	}
	s.r.IncIntegrityFailure(backend)
}

func (s integrityFailureSink) IncUnrecognized(backend string) {
	if s.r == nil {
		return
	}
	s.r.IncIntegrityClaimUnrecognized(backend)
}

// metricsMiddleware wraps next with request-duration and
// active-request bookkeeping. Each request is timed and assigned
// a status-class label (2xx/4xx/5xx).
func metricsMiddleware(reg *metrics.Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reg.IncActive()
		defer reg.DecActive()
		start := time.Now()
		sw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		dur := time.Since(start).Seconds()
		reg.ObserveRequest(r.Method, statusClass(sw.status), dur)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func statusClass(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// buildComplianceHooks constructs the residency enforcer and
// audit recorder that the s3compat handler consults. Both fields
// of the returned ComplianceHooks may be nil; the handler treats
// nil as "feature disabled".
func buildComplianceHooks(cfg config.ComplianceConfig, db *sql.DB) s3compat.ComplianceHooks {
	hooks := s3compat.ComplianceHooks{}
	if cfg.ResidencyEnabled {
		var lookup compliance.AllowlistLookup
		if len(cfg.StaticAllowlist) > 0 {
			lookup = compliance.StaticAllowlist(cfg.StaticAllowlist)
		} else if db != nil {
			lookup = postgresAllowlistLookup(db)
		}
		hooks.Residency = compliance.NewResidencyEnforcer(lookup)
	}
	if cfg.AuditEnabled {
		var store compliance.AuditStore
		if db != nil {
			store = compliance.NewPostgresAuditStore(db)
		} else {
			store = compliance.NewMemoryAuditStore()
		}
		hooks.Audit = &auditAdapter{store: store}
	}
	if cfg.LegalHoldEnabled {
		var store auth.LegalHoldStore
		if db != nil {
			pg, err := auth.NewPostgresLegalHoldStore(db)
			if err != nil {
				log.Printf("gateway: legal hold store: %v; falling back to in-memory", err)
				store = auth.NewMemoryLegalHoldStore()
			} else {
				store = pg
			}
		} else {
			store = auth.NewMemoryLegalHoldStore()
		}
		hooks.LegalHoldStore = &legalHoldAdapter{store: store}
	}
	return hooks
}

// postgresAllowlistLookup queries the tenant_country_allowlist
// table to enumerate allowed countries for a tenant.
func postgresAllowlistLookup(db *sql.DB) compliance.AllowlistLookup {
	const q = `SELECT country FROM tenant_country_allowlist WHERE tenant_id = $1`
	return func(tenantID string) ([]string, error) {
		rows, err := db.Query(q, tenantID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return nil, err
			}
			out = append(out, c)
		}
		return out, rows.Err()
	}
}

// auditAdapter forwards s3compat.AuditEntry rows to a
// compliance.AuditStore. It exists because the s3compat package
// avoids importing internal/compliance directly.
type auditAdapter struct {
	store compliance.AuditStore
}

func (a *auditAdapter) Record(ctx context.Context, e s3compat.AuditEntry) error {
	return a.store.Record(ctx, compliance.AuditEntry{
		TenantID:       e.TenantID,
		Operation:      e.Operation,
		Bucket:         e.Bucket,
		ObjectKey:      e.ObjectKey,
		PieceID:        e.PieceID,
		PieceBackend:   e.PieceBackend,
		BackendCountry: e.BackendCountry,
		Timestamp:      e.Timestamp,
		RequestID:      e.RequestID,
	})
}

// legalHoldAdapter converts auth.LegalHold records into the
// s3compat.LegalHoldEntry shape so the s3compat package does not
// have to import internal/auth.
type legalHoldAdapter struct {
	store auth.LegalHoldStore
}

func (a *legalHoldAdapter) Active(ctx context.Context, tenantID, bucket, objectKey string) ([]s3compat.LegalHoldEntry, error) {
	holds, err := a.store.Active(ctx, tenantID, bucket, objectKey)
	if err != nil {
		return nil, err
	}
	out := make([]s3compat.LegalHoldEntry, len(holds))
	for i, h := range holds {
		out[i] = s3compat.LegalHoldEntry{ID: h.ID}
	}
	return out, nil
}

// registerCellProviders enumerates active dedicated cells via
// cellops.CellRegistry and registers each as a ceph_rgw provider.
// The provider name is the cell ID, so a placement policy can
// route to a specific cell by referencing it directly.
//
// A cell whose endpoint is missing is skipped (the operator-side
// bring-up has not yet recorded one). Build failures are logged
// but never fatal — a single broken cell must not block startup.
func registerCellProviders(ctx context.Context, registry map[string]providers.StorageProvider, store cellops.CellLister, cfg config.Config) {
	if store == nil {
		return
	}
	reg := cellops.NewCellRegistry(store)
	cells, err := reg.ListActiveCells(ctx)
	if err != nil {
		log.Printf("gateway: list active cells: %v", err)
		return
	}
	for _, c := range cells {
		endpoint := cellEndpointFor(cfg, c)
		if endpoint == "" {
			log.Printf("gateway: cell %q has no endpoint; skipping", c.CellID)
			continue
		}
		bucket := cellBucketFor(cfg, c)
		ak, sk := cellCredentialsFor(cfg, c)
		p, err := ceph_rgw.New(ceph_rgw.Config{
			Endpoint:  endpoint,
			Region:    c.Region,
			Bucket:    bucket,
			AccessKey: ak,
			SecretKey: sk,
			Cell:      c.CellID,
			Country:   c.Country,
		})
		if err != nil {
			log.Printf("gateway: build cell provider %q: %v", c.CellID, err)
			continue
		}
		if _, exists := registry[c.CellID]; exists {
			log.Printf("gateway: cell provider %q already registered; skipping", c.CellID)
			continue
		}
		registry[c.CellID] = p
		log.Printf("gateway: registered cell provider %q (endpoint=%s region=%s country=%s)",
			c.CellID, endpoint, c.Region, c.Country)
	}
}

// cellEndpointFor falls back to the global ceph_rgw endpoint when
// no per-cell endpoint is wired. A future config layout will
// store per-cell endpoints in the cell row directly.
func cellEndpointFor(cfg config.Config, c cellops.CellStatus) string {
	return cfg.Providers.CephRGW.Endpoint
}

func cellBucketFor(cfg config.Config, c cellops.CellStatus) string {
	if cfg.Providers.CephRGW.Bucket != "" {
		return cfg.Providers.CephRGW.Bucket
	}
	return "cell-" + c.CellID
}

func cellCredentialsFor(cfg config.Config, c cellops.CellStatus) (string, string) {
	return cfg.Providers.CephRGW.AccessKey, cfg.Providers.CephRGW.SecretKey
}

// startCrossCellReplicator spins up the cross-cell async
// replicator when the operator enables it. The worker is gated
// on cfg.Enabled + valid SourceCellID/DestCellID; missing
// providers cause a soft skip with a log line so the gateway
// still starts.
func startCrossCellReplicator(
	ctx context.Context,
	cfg config.CrossCellConfig,
	manifests manifest_store.ManifestStore,
	registry map[string]providers.StorageProvider,
) <-chan struct{} {
	if !cfg.Enabled {
		return nil
	}
	src, ok := registry[cfg.SourceCellID]
	if !ok {
		log.Printf("cross_cell: source provider %q not registered; skipping", cfg.SourceCellID)
		return nil
	}
	dst, ok := registry[cfg.DestCellID]
	if !ok {
		log.Printf("cross_cell: dest provider %q not registered; skipping", cfg.DestCellID)
		return nil
	}
	scope := make([]cross_cell.ScopeKey, 0, len(cfg.Scope))
	for _, e := range cfg.Scope {
		scope = append(scope, cross_cell.ScopeKey{TenantID: e.TenantID, Bucket: e.Bucket})
	}
	r := cross_cell.NewReplicator(
		cross_cell.Cell{ID: cfg.SourceCellID, Manifests: manifests, Provider: src},
		cross_cell.Cell{ID: cfg.DestCellID, Manifests: manifests, Provider: dst},
		scope,
	)
	if d := time.Duration(cfg.ScanInterval); d > 0 {
		r.Interval = d
	}
	r.Logger = log.New(os.Stdout, "cross_cell ", log.LstdFlags)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	log.Printf("gateway: started cross-cell replicator src=%s dst=%s scope=%d", cfg.SourceCellID, cfg.DestCellID, len(scope))
	return done
}

// startRepairQueue runs the automated repair queue when the
// operator enables it. The queue polls a Ceph manager health
// endpoint, identifies degraded shards, and re-encodes affected
// erasure-coded manifests. The ManifestScanner used here is a
// noop — production deployments are expected to inject their own
// CRUSH-aware scanner via repair.RepairQueue.Scanner before
// calling Run; the scaffold only ensures the goroutine lifecycle
// and config wiring are in place.
func startRepairQueue(
	ctx context.Context,
	cfg config.RepairConfig,
	manifests manifest_store.ManifestStore,
	registry map[string]providers.StorageProvider,
	ec *erasure_coding.Registry,
) <-chan struct{} {
	if !cfg.Enabled {
		return nil
	}
	if cfg.CephEndpoint == "" {
		log.Printf("repair: queue enabled but ceph_endpoint is empty; skipping")
		return nil
	}
	src := &repair.CephHealthClient{
		Endpoint:  cfg.CephEndpoint,
		AuthToken: cfg.AuthToken,
	}
	q := repair.NewRepairQueue(src, repair.NoopScanner{}, manifests, registry, ec)
	if d := time.Duration(cfg.PollInterval); d > 0 {
		q.PollInterval = d
	}
	q.Logger = log.New(os.Stdout, "repair ", log.LstdFlags)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = q.Run(ctx)
	}()
	log.Printf("gateway: started repair queue endpoint=%s", cfg.CephEndpoint)
	return done
}
