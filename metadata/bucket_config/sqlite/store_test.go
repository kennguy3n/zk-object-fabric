package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
	"github.com/kennguy3n/zk-object-fabric/metadata/bucket_config"
	"github.com/kennguy3n/zk-object-fabric/metadata/cors"
	"github.com/kennguy3n/zk-object-fabric/metadata/lifecycle"
	"github.com/kennguy3n/zk-object-fabric/metadata/notification"
	"github.com/kennguy3n/zk-object-fabric/metadata/object_lock"
	"github.com/kennguy3n/zk-object-fabric/metadata/sse"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "bc.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := New(Config{DB: db})
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestSQLite_SetGetRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetVersioning(ctx, "t1", "b1"); err != nil || got != bucket_config.VersioningUnset {
		t.Fatalf("unconfigured get = (%q, %v), want (unset, nil)", got, err)
	}
	if err := s.SetVersioning(ctx, "t1", "b1", bucket_config.VersioningEnabled); err != nil {
		t.Fatalf("SetVersioning: %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != bucket_config.VersioningEnabled {
		t.Fatalf("state = %q, want Enabled", got)
	}
	// Upsert path.
	if err := s.SetVersioning(ctx, "t1", "b1", bucket_config.VersioningSuspended); err != nil {
		t.Fatalf("SetVersioning(upsert): %v", err)
	}
	if got, _ := s.GetVersioning(ctx, "t1", "b1"); got != bucket_config.VersioningSuspended {
		t.Fatalf("state = %q, want Suspended", got)
	}
}

func TestSQLite_RejectsInvalidState(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetVersioning(context.Background(), "t1", "b1", bucket_config.VersioningUnset); err == nil {
		t.Fatal("SetVersioning(unset): want error")
	}
}

func TestSQLite_ObjectLockRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetObjectLock(ctx, "t1", "b1"); err != nil || got.Enabled {
		t.Fatalf("unconfigured get = (%+v, %v), want (disabled, nil)", got, err)
	}

	// Enabled with a years default rule.
	cfg := object_lock.Config{Enabled: true, DefaultMode: object_lock.ModeCompliance, DefaultYears: 1}
	if err := s.SetObjectLock(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetObjectLock: %v", err)
	}
	if got, _ := s.GetObjectLock(ctx, "t1", "b1"); got != cfg {
		t.Fatalf("round-trip = %+v, want %+v", got, cfg)
	}

	// Upsert to enabled-no-rule clears the default fields.
	if err := s.SetObjectLock(ctx, "t1", "b1", object_lock.Config{Enabled: true}); err != nil {
		t.Fatalf("SetObjectLock(upsert): %v", err)
	}
	got, _ := s.GetObjectLock(ctx, "t1", "b1")
	if !got.Enabled || got.DefaultMode != "" || got.DefaultDays != 0 || got.DefaultYears != 0 {
		t.Fatalf("upsert result = %+v, want enabled-no-rule", got)
	}
}

func TestSQLite_ObjectLockRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	bad := object_lock.Config{Enabled: false, DefaultDays: 5} // stray rule on disabled
	if err := s.SetObjectLock(context.Background(), "t1", "b1", bad); err == nil {
		t.Fatal("SetObjectLock(invalid): want error")
	}
}

func TestSQLite_CORSRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetCORS(ctx, "t1", "b1"); err != nil || !got.Empty() {
		t.Fatalf("unconfigured get = (%+v, %v), want (empty, nil)", got, err)
	}

	cfg := cors.Config{Rules: []cors.Rule{
		{
			ID:             "rule-1",
			AllowedOrigins: []string{"https://app.example.com", "https://*.cdn.example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  3000,
		},
		{
			AllowedOrigins: []string{"*"},
			AllowedMethods: []string{"HEAD"},
		},
	}}
	if err := s.SetCORS(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetCORS: %v", err)
	}
	got, _ := s.GetCORS(ctx, "t1", "b1")
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	r0 := got.Rules[0]
	if r0.ID != "rule-1" || r0.MaxAgeSeconds != 3000 ||
		len(r0.AllowedOrigins) != 2 || r0.AllowedOrigins[1] != "https://*.cdn.example.com" ||
		r0.AllowedMethodsCSV() != "GET, PUT" || r0.ExposeHeadersCSV() != "ETag" {
		t.Fatalf("rule 0 round-trip mismatch: %+v", r0)
	}

	// Upsert replaces the rule set.
	if err := s.SetCORS(ctx, "t1", "b1", cors.Config{Rules: []cors.Rule{{
		AllowedOrigins: []string{"https://only.example.com"},
		AllowedMethods: []string{"GET"},
	}}}); err != nil {
		t.Fatalf("SetCORS(upsert): %v", err)
	}
	if got, _ := s.GetCORS(ctx, "t1", "b1"); len(got.Rules) != 1 || got.Rules[0].AllowedOrigins[0] != "https://only.example.com" {
		t.Fatalf("upsert result = %+v", got)
	}

	// Delete then idempotent re-delete.
	if err := s.DeleteCORS(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteCORS: %v", err)
	}
	if got, _ := s.GetCORS(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after delete = %+v, want empty", got)
	}
	if err := s.DeleteCORS(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteCORS (no-op): %v", err)
	}
}

func TestSQLite_CORSRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetCORS(context.Background(), "t1", "b1", cors.Config{}); err == nil {
		t.Fatal("SetCORS(empty config): want error")
	}
}

func TestSQLite_NotificationRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetNotification(ctx, "t1", "b1"); err != nil || !got.Empty() {
		t.Fatalf("unconfigured get = (%+v, %v), want (empty, nil)", got, err)
	}

	cfg := notification.Config{Rules: []notification.Rule{
		{
			ID:       "on-upload",
			Events:   []notification.EventType{notification.ObjectCreatedAll, notification.ObjectRemovedDelete},
			Endpoint: "https://hooks.example.com/s3",
			Prefix:   "logs/",
			Suffix:   ".json",
		},
		{
			ID:       "on-delete",
			Events:   []notification.EventType{notification.ObjectRemovedAll},
			Endpoint: "https://hooks.example.com/del",
		},
	}}
	if err := s.SetNotification(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetNotification: %v", err)
	}
	got, _ := s.GetNotification(ctx, "t1", "b1")
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	r0 := got.Rules[0]
	if r0.ID != "on-upload" || r0.Prefix != "logs/" || r0.Suffix != ".json" ||
		len(r0.Events) != 2 || r0.Events[0] != notification.ObjectCreatedAll {
		t.Fatalf("rule 0 round-trip mismatch: %+v", r0)
	}

	// Upsert replaces the rule set.
	if err := s.SetNotification(ctx, "t1", "b1", notification.Config{Rules: []notification.Rule{{
		ID:       "only",
		Events:   []notification.EventType{notification.ObjectCreatedPut},
		Endpoint: "https://only.example.com",
	}}}); err != nil {
		t.Fatalf("SetNotification(upsert): %v", err)
	}
	if got, _ := s.GetNotification(ctx, "t1", "b1"); len(got.Rules) != 1 || got.Rules[0].ID != "only" {
		t.Fatalf("upsert result = %+v", got)
	}

	// Empty config clears the row; re-clear is idempotent.
	if err := s.SetNotification(ctx, "t1", "b1", notification.Config{}); err != nil {
		t.Fatalf("SetNotification(clear): %v", err)
	}
	if got, _ := s.GetNotification(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after clear = %+v, want empty", got)
	}
	if err := s.SetNotification(ctx, "t1", "b1", notification.Config{}); err != nil {
		t.Fatalf("SetNotification(clear no-op): %v", err)
	}
}

func TestSQLite_NotificationRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	bad := notification.Config{Rules: []notification.Rule{{Events: []notification.EventType{notification.ObjectCreatedAll}}}}
	if err := s.SetNotification(context.Background(), "t1", "b1", bad); err == nil {
		t.Fatal("SetNotification(invalid): want error")
	}
}

func TestSQLite_LifecycleRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetLifecycle(ctx, "t1", "b1"); err != nil || !got.Empty() {
		t.Fatalf("unconfigured get = (%+v, %v), want (empty, nil)", got, err)
	}

	gt := int64(1024)
	cfg := lifecycle.Config{Rules: []lifecycle.Rule{
		{
			ID:     "expire-logs",
			Status: lifecycle.StatusEnabled,
			Filter: lifecycle.Filter{
				Prefix:                "logs/",
				Tags:                  map[string]string{"team": "infra"},
				ObjectSizeGreaterThan: &gt,
			},
			Expiration:  &lifecycle.Expiration{Days: 90},
			Transitions: []lifecycle.Transition{{Days: 30, StorageClass: "GLACIER"}},
		},
		{
			// AbortIncompleteMultipartUpload cannot be combined with a
			// tag filter, so it lives on its own prefix-only rule.
			ID:                             "abort-mpu",
			Status:                         lifecycle.StatusEnabled,
			Filter:                         lifecycle.Filter{Prefix: "tmp/"},
			AbortIncompleteMultipartUpload: &lifecycle.AbortIncompleteMultipartUpload{DaysAfterInitiation: 7},
		},
	}}
	if err := s.SetLifecycle(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetLifecycle: %v", err)
	}
	got, _ := s.GetLifecycle(ctx, "t1", "b1")
	if len(got.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(got.Rules))
	}
	r0 := got.Rules[0]
	if r0.ID != "expire-logs" || r0.Expiration == nil || r0.Expiration.Days != 90 ||
		r0.Filter.Prefix != "logs/" || r0.Filter.ObjectSizeGreaterThan == nil ||
		*r0.Filter.ObjectSizeGreaterThan != 1024 || len(r0.Transitions) != 1 ||
		r0.Filter.Tags["team"] != "infra" {
		t.Fatalf("rule 0 round-trip mismatch: %+v", r0)
	}
	if got.Rules[1].AbortIncompleteMultipartUpload == nil ||
		got.Rules[1].AbortIncompleteMultipartUpload.DaysAfterInitiation != 7 {
		t.Fatalf("rule 1 abort round-trip mismatch: %+v", got.Rules[1])
	}

	// ListLifecycle enumerates across tenants/buckets.
	if err := s.SetLifecycle(ctx, "t2", "bx", cfg); err != nil {
		t.Fatalf("SetLifecycle t2: %v", err)
	}
	entries, err := s.ListLifecycle(ctx)
	if err != nil {
		t.Fatalf("ListLifecycle: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ListLifecycle len = %d, want 2", len(entries))
	}

	// Upsert replaces the rule set.
	if err := s.SetLifecycle(ctx, "t1", "b1", lifecycle.Config{Rules: []lifecycle.Rule{{
		Status:     lifecycle.StatusEnabled,
		Expiration: &lifecycle.Expiration{Days: 1},
	}}}); err != nil {
		t.Fatalf("SetLifecycle(upsert): %v", err)
	}
	if got, _ := s.GetLifecycle(ctx, "t1", "b1"); len(got.Rules) != 1 || got.Rules[0].Expiration.Days != 1 {
		t.Fatalf("upsert result = %+v", got)
	}

	// Delete then idempotent re-delete.
	if err := s.DeleteLifecycle(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteLifecycle: %v", err)
	}
	if got, _ := s.GetLifecycle(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after delete = %+v, want empty", got)
	}
	if err := s.DeleteLifecycle(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteLifecycle (no-op): %v", err)
	}
}

func TestSQLite_EncryptionRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()

	if got, err := s.GetEncryption(ctx, "t1", "b1"); err != nil || !got.Empty() {
		t.Fatalf("unconfigured get = (%+v, %v), want (empty, nil)", got, err)
	}

	cfg := sse.Config{Algorithm: sse.AWSKMS, KMSMasterKeyID: "arn:aws:kms:k", BucketKeyEnabled: true}
	if err := s.SetEncryption(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	if got, _ := s.GetEncryption(ctx, "t1", "b1"); got != cfg {
		t.Fatalf("round-trip = %+v, want %+v", got, cfg)
	}

	// Upsert to AES256 clears the KMS key id.
	if err := s.SetEncryption(ctx, "t1", "b1", sse.Config{Algorithm: sse.AES256}); err != nil {
		t.Fatalf("SetEncryption(upsert): %v", err)
	}
	if got, _ := s.GetEncryption(ctx, "t1", "b1"); got.Algorithm != sse.AES256 || got.KMSMasterKeyID != "" || got.BucketKeyEnabled {
		t.Fatalf("upsert result = %+v, want bare AES256", got)
	}

	// Delete clears it; deleting again is a no-op.
	if err := s.DeleteEncryption(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteEncryption: %v", err)
	}
	if got, _ := s.GetEncryption(ctx, "t1", "b1"); !got.Empty() {
		t.Fatalf("after delete = %+v, want empty", got)
	}
	if err := s.DeleteEncryption(ctx, "t1", "b1"); err != nil {
		t.Fatalf("DeleteEncryption (no-op): %v", err)
	}
}

// TestSQLite_EncryptionPersistsAcrossReopen pins the embedded-profile
// guarantee: a bucket default written before a restart is still present
// after the gateway reopens the same database file.
func TestSQLite_EncryptionPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "persist.db")
	cfg := sse.Config{Algorithm: sse.AWSKMS, KMSMasterKeyID: "arn:key", BucketKeyEnabled: true}

	db1, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("open db (1): %v", err)
	}
	s1, err := New(Config{DB: db1})
	if err != nil {
		t.Fatalf("new store (1): %v", err)
	}
	if err := s1.SetEncryption(ctx, "t1", "b1", cfg); err != nil {
		t.Fatalf("SetEncryption: %v", err)
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close db (1): %v", err)
	}

	db2, err := embeddeddb.Open(path)
	if err != nil {
		t.Fatalf("reopen db (2): %v", err)
	}
	t.Cleanup(func() { _ = db2.Close() })
	s2, err := New(Config{DB: db2})
	if err != nil {
		t.Fatalf("new store (2): %v", err)
	}
	if got, _ := s2.GetEncryption(ctx, "t1", "b1"); got != cfg {
		t.Fatalf("after reopen = %+v, want %+v (config lost across restart)", got, cfg)
	}
}

func TestSQLite_EncryptionRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.SetEncryption(ctx, "t1", "b1", sse.Config{}); err == nil {
		t.Fatal("SetEncryption(empty config): want error")
	}
	if err := s.SetEncryption(ctx, "t1", "b1", sse.Config{Algorithm: sse.AES256, KMSMasterKeyID: "arn:key"}); err == nil {
		t.Fatal("SetEncryption(AES256 + KMS key): want error")
	}
}

func TestSQLite_LifecycleRejectsInvalid(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	if err := s.SetLifecycle(context.Background(), "t1", "b1", lifecycle.Config{}); err == nil {
		t.Fatal("SetLifecycle(empty config): want error")
	}
}
