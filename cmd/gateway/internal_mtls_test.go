package main

import (
	"net/url"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/config"
)

func testInternalTLS() config.InternalTLSConfig {
	return config.InternalTLSConfig{
		Enabled:  true,
		CertFile: "/etc/zkof/tls/client.pem",
		KeyFile:  "/etc/zkof/tls/client-key.pem",
		CAFile:   "/etc/zkof/tls/ca.pem",
	}
}

func TestApplyInternalTLSToPostgresDSN_URLForm(t *testing.T) {
	c := testInternalTLS()

	t.Run("adds cert params and defaults sslmode to verify-full", func(t *testing.T) {
		got, weak, err := applyInternalTLSToPostgresDSN("postgres://u:p@db.internal:5432/meta", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if weak {
			t.Error("weak = true; default sslmode should be verify-full (verifying)")
		}
		q := mustQuery(t, got)
		assertParam(t, q, "sslmode", "verify-full")
		assertParam(t, q, "sslcert", c.CertFile)
		assertParam(t, q, "sslkey", c.KeyFile)
		assertParam(t, q, "sslrootcert", c.CAFile)
	})

	t.Run("preserves operator sslmode and cert paths", func(t *testing.T) {
		in := "postgres://u@db.internal/meta?sslmode=verify-ca&sslrootcert=/custom/ca.pem"
		got, weak, err := applyInternalTLSToPostgresDSN(in, c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if weak {
			t.Error("verify-ca should not be flagged weak")
		}
		q := mustQuery(t, got)
		assertParam(t, q, "sslmode", "verify-ca")
		assertParam(t, q, "sslrootcert", "/custom/ca.pem") // operator value wins
		assertParam(t, q, "sslcert", c.CertFile)           // filled in
	})

	t.Run("flags non-verifying operator sslmode", func(t *testing.T) {
		got, weak, err := applyInternalTLSToPostgresDSN("postgres://u@db/meta?sslmode=require", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !weak {
			t.Error("sslmode=require should be flagged weak (no server verification)")
		}
		q := mustQuery(t, got)
		assertParam(t, q, "sslmode", "require") // preserved, not overridden
	})

	t.Run("explicitly-empty operator param is preserved, not overwritten", func(t *testing.T) {
		// "?sslcert=" is a deliberate empty value; q.Has must keep it so
		// the operator's explicit choice wins over the injected cert.
		got, _, err := applyInternalTLSToPostgresDSN("postgres://u@db/meta?sslcert=", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		q := mustQuery(t, got)
		if !q.Has("sslcert") || q.Get("sslcert") != "" {
			t.Errorf("explicit empty sslcert should be preserved, got %q", q.Get("sslcert"))
		}
		// sslkey/sslrootcert (truly absent) are still filled in.
		assertParam(t, q, "sslkey", c.KeyFile)
	})

	t.Run("postgresql scheme also handled", func(t *testing.T) {
		got, _, err := applyInternalTLSToPostgresDSN("postgresql://u@db/meta", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(got, "sslmode=verify-full") {
			t.Errorf("expected sslmode default, got %q", got)
		}
	})
}

func TestApplyInternalTLSToPostgresDSN_KeywordForm(t *testing.T) {
	c := testInternalTLS()

	t.Run("appends omitted params and defaults sslmode", func(t *testing.T) {
		got, weak, err := applyInternalTLSToPostgresDSN("host=db.internal user=u dbname=meta", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if weak {
			t.Error("default verify-full should not be weak")
		}
		// appended cert paths are single-quoted (libpq keyword form)
		for _, want := range []string{"sslmode=verify-full", "sslcert='" + c.CertFile + "'", "sslkey='" + c.KeyFile + "'", "sslrootcert='" + c.CAFile + "'"} {
			if !strings.Contains(got, want) {
				t.Errorf("result %q missing %q", got, want)
			}
		}
	})

	t.Run("paths with whitespace are quoted so they parse as one value", func(t *testing.T) {
		spaced := config.InternalTLSConfig{
			Enabled:  true,
			CertFile: "/etc/zkof/my certs/client.pem",
			KeyFile:  "/etc/zkof/my certs/client-key.pem",
			CAFile:   "/etc/zkof/my certs/ca.pem",
		}
		got, _, err := applyInternalTLSToPostgresDSN("host=db user=u", spaced)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		// The spaced path must be wrapped in quotes, not appended bare
		// (which would truncate at the first space).
		if !strings.Contains(got, "sslcert='/etc/zkof/my certs/client.pem'") {
			t.Errorf("spaced cert path not quoted as a single value: %q", got)
		}
		// keywordDSNValue must round-trip the quoted value back intact.
		if v, ok := keywordDSNValue(got, "sslcert"); !ok || v != spaced.CertFile {
			t.Errorf("keywordDSNValue(sslcert) = (%q, %v), want (%q, true)", v, ok, spaced.CertFile)
		}
	})

	t.Run("backslash and quote in cert path round-trip through the DSN", func(t *testing.T) {
		tricky := config.InternalTLSConfig{
			Enabled:  true,
			CertFile: `/etc/o'brien/a\b/client.pem`,
			KeyFile:  "/etc/zkof/key.pem",
			CAFile:   "/etc/zkof/ca.pem",
		}
		got, _, err := applyInternalTLSToPostgresDSN("host=db user=u", tricky)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if v, ok := keywordDSNValue(got, "sslcert"); !ok || v != tricky.CertFile {
			t.Errorf("sslcert round-trip = (%q, %v), want (%q, true)", v, ok, tricky.CertFile)
		}
	})

	t.Run("does not duplicate existing keys", func(t *testing.T) {
		in := "host=db user=u sslmode=verify-full sslcert=/op/c.pem"
		got, weak, err := applyInternalTLSToPostgresDSN(in, c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if weak {
			t.Error("verify-full not weak")
		}
		if strings.Count(got, "sslmode=") != 1 {
			t.Errorf("sslmode duplicated: %q", got)
		}
		if strings.Count(got, "sslcert=") != 1 || !strings.Contains(got, "sslcert=/op/c.pem") {
			t.Errorf("operator sslcert should win and not duplicate: %q", got)
		}
	})

	t.Run("flags non-verifying operator sslmode", func(t *testing.T) {
		_, weak, err := applyInternalTLSToPostgresDSN("host=db user=u sslmode=disable", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !weak {
			t.Error("sslmode=disable should be flagged weak")
		}
	})

	t.Run("present-but-empty sslmode is upgraded to verify-full like URL form", func(t *testing.T) {
		for _, in := range []string{"host=db user=u sslmode=", "host=db user=u sslmode=''"} {
			got, weak, err := applyInternalTLSToPostgresDSN(in, c)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if weak {
				t.Errorf("%q: empty sslmode should be upgraded to verifying, got weak=true", in)
			}
			// libpq honours the last duplicate, so verify-full must be appended.
			if !strings.HasSuffix(got, "sslmode=verify-full") {
				t.Errorf("%q: expected verify-full appended, got %q", in, got)
			}
		}
	})

	t.Run("quoted value is parsed", func(t *testing.T) {
		_, weak, err := applyInternalTLSToPostgresDSN("host=db sslmode='verify-ca'", c)
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if weak {
			t.Error("quoted verify-ca should be recognised as verifying")
		}
	})
}

func TestNonVerifyingPostgresSSLMode(t *testing.T) {
	verifying := []string{"verify-ca", "verify-full", "VERIFY-FULL", " verify-ca "}
	for _, m := range verifying {
		if nonVerifyingPostgresSSLMode(m) {
			t.Errorf("%q should be verifying", m)
		}
	}
	weak := []string{"", "disable", "allow", "prefer", "require"}
	for _, m := range weak {
		if !nonVerifyingPostgresSSLMode(m) {
			t.Errorf("%q should be non-verifying", m)
		}
	}
}

func TestKeywordDSNValue(t *testing.T) {
	cases := []struct {
		dsn, key, want string
		present        bool
	}{
		{"host=db sslmode=require", "sslmode", "require", true},
		{"sslmode=verify-full host=db", "sslmode", "verify-full", true},
		{"host=db sslmode = prefer", "sslmode", "prefer", true},
		{"host=db sslmode='verify-ca'", "sslmode", "verify-ca", true},
		{"host=db user=u", "sslmode", "", false},
		// must not match a substring key (e.g. xsslmode)
		{"host=db xsslmode=require", "sslmode", "", false},
		// a bare "key=" is present with an empty value
		{"host=db sslmode=", "sslmode", "", true},
		// quoted empty value is present and empty
		{"host=db sslmode=''", "sslmode", "", true},
		// later duplicate wins, matching libpq
		{"sslmode=require sslmode=verify-full", "sslmode", "verify-full", true},
		// a key name appearing *inside* another key's quoted value must
		// NOT be picked up (quoting context is respected)
		{"sslcert='/etc/x sslkey=fake' host=db", "sslkey", "", false},
		{"sslcert='/etc/x sslkey=fake' host=db", "sslcert", "/etc/x sslkey=fake", true},
		// backslash-escaped quote inside a quoted value round-trips
		{`sslcert='/etc/o\'brien/c.pem'`, "sslcert", "/etc/o'brien/c.pem", true},
		// escaped backslash round-trips to a single backslash
		{`sslcert='/etc/a\\b/c.pem'`, "sslcert", `/etc/a\b/c.pem`, true},
	}
	for _, tc := range cases {
		got, present := keywordDSNValue(tc.dsn, tc.key)
		if present != tc.present || got != tc.want {
			t.Errorf("keywordDSNValue(%q, %q) = (%q, %v), want (%q, %v)", tc.dsn, tc.key, got, present, tc.want, tc.present)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse result DSN %q: %v", raw, err)
	}
	return u.Query()
}

func assertParam(t *testing.T, q url.Values, key, want string) {
	t.Helper()
	if got := q.Get(key); got != want {
		t.Errorf("param %s = %q, want %q", key, got, want)
	}
}
