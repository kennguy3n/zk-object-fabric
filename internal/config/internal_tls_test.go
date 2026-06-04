package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestInternalTLSConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     InternalTLSConfig
		wantErr bool
	}{
		{
			name: "disabled is always valid even when empty",
			cfg:  InternalTLSConfig{Enabled: false},
		},
		{
			name: "disabled ignores half-populated fields",
			cfg:  InternalTLSConfig{Enabled: false, CertFile: "only-cert.pem"},
		},
		{
			name: "enabled with all three set",
			cfg:  InternalTLSConfig{Enabled: true, CertFile: "c.pem", KeyFile: "k.pem", CAFile: "ca.pem"},
		},
		{
			name:    "enabled missing cert",
			cfg:     InternalTLSConfig{Enabled: true, KeyFile: "k.pem", CAFile: "ca.pem"},
			wantErr: true,
		},
		{
			name:    "enabled missing key",
			cfg:     InternalTLSConfig{Enabled: true, CertFile: "c.pem", CAFile: "ca.pem"},
			wantErr: true,
		},
		{
			name:    "enabled missing ca",
			cfg:     InternalTLSConfig{Enabled: true, CertFile: "c.pem", KeyFile: "k.pem"},
			wantErr: true,
		},
		{
			name:    "enabled whitespace-only counts as missing",
			cfg:     InternalTLSConfig{Enabled: true, CertFile: "  ", KeyFile: "k.pem", CAFile: "ca.pem"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestBuildClientTLSConfig(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, caPath := writeTestKeypair(t, dir)

	t.Run("disabled returns error", func(t *testing.T) {
		_, err := InternalTLSConfig{Enabled: false}.BuildClientTLSConfig()
		if err == nil {
			t.Fatal("expected error when building from a disabled config")
		}
	})

	t.Run("valid material loads", func(t *testing.T) {
		c := InternalTLSConfig{Enabled: true, CertFile: certPath, KeyFile: keyPath, CAFile: caPath}
		got, err := c.BuildClientTLSConfig()
		if err != nil {
			t.Fatalf("BuildClientTLSConfig() error = %v", err)
		}
		if got.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %x, want TLS 1.2 (%x)", got.MinVersion, tls.VersionTLS12)
		}
		if len(got.Certificates) != 1 {
			t.Fatalf("Certificates = %d, want 1", len(got.Certificates))
		}
		if got.RootCAs == nil {
			t.Error("RootCAs is nil; CA pinning would fall back to system roots")
		}
	})

	t.Run("missing cert file errors with path", func(t *testing.T) {
		c := InternalTLSConfig{Enabled: true, CertFile: filepath.Join(dir, "nope.pem"), KeyFile: keyPath, CAFile: caPath}
		if _, err := c.BuildClientTLSConfig(); err == nil {
			t.Fatal("expected error for missing cert file")
		}
	})

	t.Run("ca file without PEM certs errors", func(t *testing.T) {
		empty := filepath.Join(dir, "empty-ca.pem")
		if err := os.WriteFile(empty, []byte("not a pem"), 0o600); err != nil {
			t.Fatal(err)
		}
		c := InternalTLSConfig{Enabled: true, CertFile: certPath, KeyFile: keyPath, CAFile: empty}
		if _, err := c.BuildClientTLSConfig(); err == nil {
			t.Fatal("expected error for CA file with no certificates")
		}
	})
}

// writeTestKeypair writes a self-signed cert/key pair (reused as both
// the client cert and the CA bundle for the loader tests) and returns
// their paths.
func writeTestKeypair(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "zkof-internal-mtls-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	certPath = filepath.Join(dir, "client.pem")
	keyPath = filepath.Join(dir, "client-key.pem")
	caPath = filepath.Join(dir, "ca.pem")
	for path, data := range map[string][]byte{certPath: certPEM, keyPath: keyPEM, caPath: certPEM} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return certPath, keyPath, caPath
}
