// SPDX-License-Identifier: Apache-2.0
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kennguy3n/zk-object-fabric/internal/auth"
	"github.com/kennguy3n/zk-object-fabric/internal/config"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// generateSelfSignedCert writes a freshly-generated self-signed
// P-256 ECDSA cert + key to disk under dir and returns the cert
// path, key path, and the cert (as *x509.Certificate) so callers
// can pin it as the only trusted root on the test client.
func generateSelfSignedCert(t *testing.T, dir string) (certPath, keyPath string, cert *x509.Certificate) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("rand serial: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "zkof-test"},
		NotBefore:             time.Now().Add(-1 * time.Hour),
		NotAfter:              time.Now().Add(1 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.IPv6loopback},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("x509.CreateCertificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("x509.ParseCertificate: %v", err)
	}
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath, parsed
}

// TestStartListener_TLS_Roundtrip pins the TLS-enabled branch of
// startListener: an HTTPS handshake against the self-signed cert
// succeeds and the request body is delivered to the handler.
func TestStartListener_TLS_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, cert := generateSelfSignedCert(t, dir)

	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "tls-ok")
	})
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv.Addr = addr

	done := make(chan error, 1)
	go func() {
		done <- startListener(srv, config.TLSConfig{
			CertPath:   certPath,
			KeyPath:    keyPath,
			MinVersion: "1.2",
		}, "production", "gateway-test")
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})

	// Wait until the listener is actually accepting.
	if err := waitDial(t, addr, 2*time.Second); err != nil {
		t.Fatalf("wait dial: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:    pool,
				ServerName: "localhost",
				MinVersion: tls.VersionTLS12,
			},
		},
	}

	resp, err := client.Get("https://" + strings.Replace(addr, "127.0.0.1", "localhost", 1) + "/probe")
	if err != nil {
		t.Fatalf("https GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tls-ok" {
		t.Fatalf("body = %q, want %q", body, "tls-ok")
	}

	// Server's negotiated TLS config came from BuildGoTLSConfig
	// (MinVersion=1.2 by configuration).
	if srv.TLSConfig == nil {
		t.Fatal("srv.TLSConfig is nil; startListener should have installed it")
	}
	if srv.TLSConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("srv.TLSConfig.MinVersion = %#x, want %#x", srv.TLSConfig.MinVersion, tls.VersionTLS12)
	}
}

// TestStartListener_TLSDisabled_PlainHTTP pins the
// TLS-disabled branch of startListener: a plain HTTP request
// against the same handler succeeds, and srv.TLSConfig is left
// untouched.
func TestStartListener_TLSDisabled_PlainHTTP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/probe", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "plain-ok")
	})
	srv := &http.Server{Handler: mux}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	srv.Addr = addr

	done := make(chan error, 1)
	go func() {
		// env="development" so the production-no-TLS WARN
		// path is not exercised here (that path is covered
		// separately).
		done <- startListener(srv, config.TLSConfig{}, "development", "gateway-test")
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		<-done
	})

	if err := waitDial(t, addr, 2*time.Second); err != nil {
		t.Fatalf("wait dial: %v", err)
	}

	resp, err := http.Get("http://" + addr + "/probe")
	if err != nil {
		t.Fatalf("http GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "plain-ok" {
		t.Fatalf("body = %q, want %q", body, "plain-ok")
	}
	// Note: we deliberately do NOT assert srv.TLSConfig == nil
	// here. Go's net/http package pre-allocates an empty
	// TLSConfig during HTTP/2 negotiation setup (called from
	// Serve via onceSetNextProtoDefaults) even on plain HTTP
	// listeners, so the assertion that mattered -- startListener
	// did not run BuildGoTLSConfig -- is verified indirectly by
	// the plain HTTP GET succeeding above.
}

// TestStartListener_TLS_InvalidMinVersion verifies that a bad
// min_version is reported through the error return rather than
// silently downgrading to the Go default.
func TestStartListener_TLS_InvalidMinVersion(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := generateSelfSignedCert(t, dir)
	srv := &http.Server{Handler: http.NewServeMux(), Addr: "127.0.0.1:0"}
	err := startListener(srv, config.TLSConfig{
		CertPath:   certPath,
		KeyPath:    keyPath,
		MinVersion: "1.0",
	}, "production", "gateway-test")
	if err == nil {
		t.Fatal("startListener: want error for min_version=1.0, got nil")
	}
	if !strings.Contains(err.Error(), "min_version") {
		t.Fatalf("error = %v, want to mention min_version", err)
	}
}

// TestStartListener_TLS_PartialConfigRejected verifies that a
// partial TLS config (exactly one of cert_path or key_path set) is
// rejected as a startup error instead of silently falling through
// to plain HTTP. This is the load-bearing safety check against the
// "operator typoed key_path and lost TLS entirely" failure mode.
func TestStartListener_TLS_PartialConfigRejected(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, _ := generateSelfSignedCert(t, dir)
	cases := []struct {
		name      string
		tls       config.TLSConfig
		wantMatch string
	}{
		{
			"cert_path set, key_path empty",
			config.TLSConfig{CertPath: certPath},
			"cert_path is set but key_path is empty",
		},
		{
			"key_path set, cert_path empty",
			config.TLSConfig{KeyPath: keyPath},
			"key_path is set but cert_path is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &http.Server{Handler: http.NewServeMux(), Addr: "127.0.0.1:0"}
			err := startListener(srv, tc.tls, "production", "gateway-test")
			if err == nil {
				t.Fatalf("startListener: want error for partial TLS config, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMatch) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tc.wantMatch)
			}
			// The server must NOT have started — TLSConfig
			// is unset, no listener was opened, so a follow-up
			// startListener call on the same struct cannot
			// race with a leaked goroutine.
			if srv.TLSConfig != nil {
				t.Errorf("srv.TLSConfig was set even though Validate rejected the config; want nil")
			}
		})
	}
}

// TestTLSVersionLabel verifies the log helper renders known
// versions correctly and the default "0x%04x" branch fires for an
// unrecognised version constant — so a future MinTLSVersion
// extension that forgets to update tlsVersionLabel surfaces the
// raw constant in log lines instead of misreporting "1.2".
func TestTLSVersionLabel(t *testing.T) {
	if got := tlsVersionLabel(tls.VersionTLS13); got != "1.3" {
		t.Errorf("tlsVersionLabel(VersionTLS13) = %q, want 1.3", got)
	}
	if got := tlsVersionLabel(tls.VersionTLS12); got != "1.2" {
		t.Errorf("tlsVersionLabel(VersionTLS12) = %q, want 1.2", got)
	}
	if got := tlsVersionLabel(0x9999); got != "0x9999" {
		t.Errorf("tlsVersionLabel(unknown) = %q, want 0x9999", got)
	}
}

// loadedTenantStore builds a MemoryTenantStore pre-loaded with a
// single binding so enforceProductionAuth tests can simulate
// "--tenants file provided with at least one binding".
func loadedTenantStore(t *testing.T) *auth.MemoryTenantStore {
	t.Helper()
	s := auth.NewMemoryTenantStore()
	if err := s.CreateTenant(tenant.Tenant{ID: "t1", Name: "t1"}); err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	if err := s.AddBinding(auth.TenantBinding{
		AccessKey: "AKID-test",
		SecretKey: "super-secret",
		Tenant:    tenant.Tenant{ID: "t1", Name: "t1"},
	}); err != nil {
		t.Fatalf("AddBinding: %v", err)
	}
	return s
}

// TestCheckProductionAuth_NonProduction: in development mode the
// guard must not fire even with no metadata DB and no bindings.
func TestCheckProductionAuth_NonProduction(t *testing.T) {
	for _, env := range []string{"development", "", "staging"} {
		if err := checkProductionAuth(env, nil, auth.NewMemoryTenantStore()); err != nil {
			t.Errorf("checkProductionAuth(%q, nil, empty store) = %v; want nil", env, err)
		}
	}
}

// TestCheckProductionAuth_ProductionWithBindings: in production
// with --tenants-loaded bindings the guard must not fire.
func TestCheckProductionAuth_ProductionWithBindings(t *testing.T) {
	if err := checkProductionAuth("production", nil, loadedTenantStore(t)); err != nil {
		t.Errorf("checkProductionAuth(production, nil, loaded store) = %v; want nil", err)
	}
}

// TestCheckProductionAuth_ProductionWithMetadataDB: in production
// with a Postgres-backed tenant store (even with zero bindings —
// a fresh deploy waiting for the console signup flow) the guard
// must not fire. We can't easily construct a real *sql.DB here,
// but a non-nil pointer is enough: the guard short-circuits on
// metadataDB != nil before touching the store.
func TestCheckProductionAuth_ProductionWithMetadataDB(t *testing.T) {
	if err := checkProductionAuth("production", &sql.DB{}, auth.NewMemoryTenantStore()); err != nil {
		t.Errorf("checkProductionAuth(production, db, empty store) = %v; want nil", err)
	}
}

// TestCheckProductionAuth_ProductionFails verifies the error
// path: production with no metadata DB and an empty tenant store
// must return errProductionAuthRequired so the startup wrapper
// can refuse to boot. No subprocess re-exec needed because the
// underlying check returns an error instead of calling
// log.Fatalf.
func TestCheckProductionAuth_ProductionFails(t *testing.T) {
	err := checkProductionAuth("production", nil, auth.NewMemoryTenantStore())
	if err == nil {
		t.Fatalf("checkProductionAuth(production, nil, empty store) = nil; want errProductionAuthRequired")
	}
	if !errors.Is(err, errProductionAuthRequired) {
		t.Fatalf("checkProductionAuth returned %v; want errors.Is(_, errProductionAuthRequired)", err)
	}
	if !strings.Contains(err.Error(), "no tenant bindings are configured") {
		t.Errorf("error message = %q; want to mention 'no tenant bindings are configured'", err.Error())
	}
}

// TestCheckAllTLSConfigs_AllValid_NoError pins the happy path:
// every listener TLS block validates clean. Covers the
// gateway-only, gateway+console, gateway+console+health
// combinations operators actually deploy.
func TestCheckAllTLSConfigs_AllValid_NoError(t *testing.T) {
	cases := []struct {
		name string
		in   config.Config
	}{
		{
			"only gateway listener configured",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
			},
		},
		{
			"gateway + console listeners (TLS off)",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{}},
			},
		},
		{
			"gateway + console + health (TLS off)",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{}},
				Health:  config.HealthConfig{ListenAddr: ":9091", TLS: config.TLSConfig{}},
			},
		},
		{
			"gateway TLS enabled, console+health plain",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem"}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{}},
				Health:  config.HealthConfig{ListenAddr: ":9091", TLS: config.TLSConfig{}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkAllTLSConfigs(tc.in); err != nil {
				t.Fatalf("checkAllTLSConfigs: unexpected error %v", err)
			}
		})
	}
}

// TestCheckAllTLSConfigs_PartialPerListener is the regression
// guard for the soft-failure bug: when the console or health
// listener has a partial TLS config (exactly one of cert_path /
// key_path set), checkAllTLSConfigs MUST return an error that
// names the offending listener. Without this pre-flight check
// the startup would proceed and the partial-TLS console/health
// listener would silently fail in its goroutine — log.Printf
// only, not log.Fatalf — leaving the gateway running with the
// listener mysteriously absent.
//
// The matrix covers every listener × every "exactly one path set"
// shape because the wrong listener name in the wrapped error would
// be invisible from a passing test that only checks one listener.
func TestCheckAllTLSConfigs_PartialPerListener(t *testing.T) {
	cases := []struct {
		name         string
		in           config.Config
		wantListener string // substring the error MUST contain
	}{
		{
			"gateway cert_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem"}},
			},
			"gateway.tls",
		},
		{
			"gateway key_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{KeyPath: "/etc/tls/key.pem"}},
			},
			"gateway.tls",
		},
		{
			"console cert_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem"}},
			},
			"console.tls",
		},
		{
			"console key_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{KeyPath: "/etc/tls/key.pem"}},
			},
			"console.tls",
		},
		{
			"health cert_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Health:  config.HealthConfig{ListenAddr: ":9091", TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem"}},
			},
			"health.tls",
		},
		{
			"health key_path only is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Health:  config.HealthConfig{ListenAddr: ":9091", TLS: config.TLSConfig{KeyPath: "/etc/tls/key.pem"}},
			},
			"health.tls",
		},
		{
			"console bad min_version is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Console: config.ConsoleConfig{ListenAddr: ":9090", TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem", MinVersion: "1.0"}},
			},
			"console.tls",
		},
		{
			"health bad min_version is rejected",
			config.Config{
				Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
				Health:  config.HealthConfig{ListenAddr: ":9091", TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem", KeyPath: "/etc/tls/key.pem", MinVersion: "1.0"}},
			},
			"health.tls",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkAllTLSConfigs(tc.in)
			if err == nil {
				t.Fatalf("checkAllTLSConfigs: want error containing %q, got nil", tc.wantListener)
			}
			if !strings.Contains(err.Error(), tc.wantListener) {
				t.Errorf("checkAllTLSConfigs error = %q; want substring %q (so operators see which listener is misconfigured)", err.Error(), tc.wantListener)
			}
		})
	}
}

// TestCheckAllTLSConfigs_SkipsDisabledListeners covers the
// branch where Console.ListenAddr == "" (or Health.ListenAddr
// == "") — those listeners are not started at all, so their TLS
// blocks must not be validated (a leftover stub TLS config in a
// config file with the listener disabled is not an error). The
// gateway data-plane listener always exists so its TLS block is
// always validated.
func TestCheckAllTLSConfigs_SkipsDisabledListeners(t *testing.T) {
	cfg := config.Config{
		Gateway: config.GatewayConfig{TLS: config.TLSConfig{}},
		// Console disabled (empty ListenAddr) but with a
		// partial TLS config that WOULD fail validation if
		// the listener were active.
		Console: config.ConsoleConfig{ListenAddr: "", TLS: config.TLSConfig{CertPath: "/etc/tls/cert.pem"}},
		// Same for health.
		Health: config.HealthConfig{ListenAddr: "", TLS: config.TLSConfig{KeyPath: "/etc/tls/key.pem"}},
	}
	if err := checkAllTLSConfigs(cfg); err != nil {
		t.Fatalf("checkAllTLSConfigs with disabled console/health: unexpected error %v (validation should be skipped for listeners with empty ListenAddr)", err)
	}
}

// TestIsLocalFileCMK pins the (uri, holder) detection used by the
// production-mode CMK warning.
func TestIsLocalFileCMK(t *testing.T) {
	cases := []struct {
		name   string
		uri    string
		holder string
		want   bool
	}{
		{"empty uri local holder", "", "gateway_hsm", true},
		{"cmk://local/... local holder", "cmk://local/path/to/key.json", "gateway_hsm", true},
		{"kms arn aws holder", "arn:aws:kms:us-west-2:1234:key/abc", "aws_kms", false},
		{"vault transit", "vault://transit/k", "vault_transit", false},
		{"local-looking uri but vault holder", "cmk://local/path", "vault_transit", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLocalFileCMK(tc.uri, tc.holder); got != tc.want {
				t.Fatalf("isLocalFileCMK(%q, %q) = %v, want %v", tc.uri, tc.holder, got, tc.want)
			}
		})
	}
}

// TestWarnProductionLocalCMK_LogsWhenProduction asserts the
// SECURITY warning is emitted (captured via log.SetOutput) only in
// production with the local file wrapper.
func TestWarnProductionLocalCMK_LogsWhenProduction(t *testing.T) {
	cases := []struct {
		name   string
		env    string
		uri    string
		holder string
		want   bool
	}{
		{"production + local file = warn", "production", "cmk://local/k", "gateway_hsm", true},
		{"production + empty uri local = warn", "production", "", "gateway_hsm", true},
		{"production + kms = quiet", "production", "kms://abc", "aws_kms", false},
		{"development + local file = quiet", "development", "cmk://local/k", "gateway_hsm", false},
		{"staging + local file = quiet", "staging", "cmk://local/k", "gateway_hsm", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := captureLog(t, func() {
				warnProductionLocalCMK(tc.env, tc.uri, tc.holder)
			})
			got := strings.Contains(out, "SECURITY: using local file CMK")
			if got != tc.want {
				t.Fatalf("warn fired = %v, want %v (log=%q)", got, tc.want, out)
			}
		})
	}
}
