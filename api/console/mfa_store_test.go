package console

import (
	"path/filepath"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/embeddeddb"
)

// mfaStoreFactory builds a fresh, empty MFA store.
type mfaStoreFactory func(t *testing.T) MFAStore

func memoryMFAFactory(t *testing.T) MFAStore {
	t.Helper()
	return NewMemoryMFAStore()
}

func sqliteMFAFactory(t *testing.T) MFAStore {
	t.Helper()
	db, err := embeddeddb.Open(filepath.Join(t.TempDir(), "mfa.db"))
	if err != nil {
		t.Fatalf("open embedded db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s, err := NewSQLiteMFAStore(db)
	if err != nil {
		t.Fatalf("new sqlite mfa store: %v", err)
	}
	return s
}

// TestMFAStoreContract runs one behavioural suite against every MFAStore
// implementation so the Memory and SQLite backends are guaranteed to
// agree. The Postgres backend runs the same suite in
// postgres_mfa_store_test.go when a DSN is configured.
func TestMFAStoreContract(t *testing.T) {
	t.Parallel()
	backends := []struct {
		name string
		make mfaStoreFactory
	}{
		{"memory", memoryMFAFactory},
		{"sqlite", sqliteMFAFactory},
	}
	for _, b := range backends {
		b := b
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			runMFAStoreContract(t, b.make)
		})
	}
}

func runMFAStoreContract(t *testing.T, newStore mfaStoreFactory) {
	t.Run("GetMissingIsNotFound", func(t *testing.T) {
		s := newStore(t)
		if _, ok, err := s.GetMFA("t-none"); err != nil || ok {
			t.Fatalf("GetMFA(missing) = (_, %v, %v); want (_, false, nil)", ok, err)
		}
	})

	t.Run("EnrollIsPendingNotActive", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-1", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		rec, ok, err := s.GetMFA("t-1")
		if err != nil || !ok {
			t.Fatalf("GetMFA after enroll = (_, %v, %v)", ok, err)
		}
		if rec.Active {
			t.Fatal("enrollment should be pending (Active=false) before activation")
		}
		if rec.Secret != "SECRET" {
			t.Fatalf("stored secret = %q, want SECRET", rec.Secret)
		}
	})

	t.Run("EnrollRejectsEmptyArgs", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("", "SECRET"); err == nil {
			t.Fatal("BeginEnrollment with empty tenant: want error")
		}
		if err := s.BeginEnrollment("t-x", ""); err == nil {
			t.Fatal("BeginEnrollment with empty secret: want error")
		}
	})

	t.Run("ActivateRequiresPending", func(t *testing.T) {
		s := newStore(t)
		if err := s.Activate("t-missing", 100, []string{"h1"}); err != errMFANotEnrolled {
			t.Fatalf("Activate(no enrollment) err = %v; want errMFANotEnrolled", err)
		}
	})

	t.Run("ActivateFlipsActiveAndSetsWatermark", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-2", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		hashes := []string{hashRecoveryCode("AAAA"), hashRecoveryCode("BBBB")}
		if err := s.Activate("t-2", 555, hashes); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		rec, ok, err := s.GetMFA("t-2")
		if err != nil || !ok {
			t.Fatalf("GetMFA after activate = (_, %v, %v)", ok, err)
		}
		if !rec.Active {
			t.Fatal("record should be Active after Activate")
		}
		if rec.LastStep != 555 {
			t.Fatalf("LastStep = %d, want 555 (the activating step)", rec.LastStep)
		}
		if rec.RecoveryRemaining != 2 {
			t.Fatalf("RecoveryRemaining = %d, want 2", rec.RecoveryRemaining)
		}
	})

	t.Run("ActivateOnActiveIsRejectedAndPreservesCodes", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-2b", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		first := []string{hashRecoveryCode("AAAA"), hashRecoveryCode("BBBB")}
		if err := s.Activate("t-2b", 555, first); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		// A second activation (e.g. two enroll/activate requests
		// racing on the same pending row) must not clobber the
		// already-issued recovery codes — it is rejected outright.
		second := []string{hashRecoveryCode("CCCC")}
		if err := s.Activate("t-2b", 999, second); err != errMFANotEnrolled {
			t.Fatalf("re-Activate err = %v; want errMFANotEnrolled", err)
		}
		rec, _, err := s.GetMFA("t-2b")
		if err != nil {
			t.Fatalf("GetMFA: %v", err)
		}
		// Watermark and recovery set must be the first activation's.
		if rec.LastStep != 555 {
			t.Fatalf("LastStep = %d, want 555 (unchanged by rejected re-activate)", rec.LastStep)
		}
		if rec.RecoveryRemaining != 2 {
			t.Fatalf("RecoveryRemaining = %d, want 2 (first set preserved)", rec.RecoveryRemaining)
		}
		// The first set's codes must still be usable; the rejected
		// set's code must never have been stored.
		if ok, err := s.ConsumeRecoveryCode("t-2b", hashRecoveryCode("CCCC")); err != nil || ok {
			t.Fatalf("ConsumeRecoveryCode(rejected set) = (%v, %v); want (false, nil)", ok, err)
		}
		if ok, err := s.ConsumeRecoveryCode("t-2b", hashRecoveryCode("AAAA")); err != nil || !ok {
			t.Fatalf("ConsumeRecoveryCode(first set) = (%v, %v); want (true, nil)", ok, err)
		}
	})

	t.Run("EnrollOnActiveIsRejected", func(t *testing.T) {
		s := newStore(t)
		mustEnrollAndActivate(t, s, "t-3", "SECRET", 1)
		if err := s.BeginEnrollment("t-3", "OTHERSECRET"); err != errMFAAlreadyActive {
			t.Fatalf("BeginEnrollment on active err = %v; want errMFAAlreadyActive", err)
		}
		// The original secret must survive the rejected re-enroll.
		rec, _, _ := s.GetMFA("t-3")
		if rec.Secret != "SECRET" {
			t.Fatalf("secret after rejected re-enroll = %q, want SECRET", rec.Secret)
		}
	})

	t.Run("MarkTOTPStepAdvancesForwardOnly", func(t *testing.T) {
		s := newStore(t)
		mustEnrollAndActivate(t, s, "t-4", "SECRET", 100)
		// A newer step is accepted and advances the watermark.
		if ok, err := s.MarkTOTPStep("t-4", 101); err != nil || !ok {
			t.Fatalf("MarkTOTPStep(101) = (%v, %v); want (true, nil)", ok, err)
		}
		// Replaying the same step (or an older one) is rejected.
		if ok, err := s.MarkTOTPStep("t-4", 101); err != nil || ok {
			t.Fatalf("MarkTOTPStep(replay 101) = (%v, %v); want (false, nil)", ok, err)
		}
		if ok, err := s.MarkTOTPStep("t-4", 50); err != nil || ok {
			t.Fatalf("MarkTOTPStep(older 50) = (%v, %v); want (false, nil)", ok, err)
		}
	})

	t.Run("MarkTOTPStepFalseWhenNotActive", func(t *testing.T) {
		s := newStore(t)
		// Pending (not yet active) enrollment: a step must not register.
		if err := s.BeginEnrollment("t-5", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		if ok, err := s.MarkTOTPStep("t-5", 10); err != nil || ok {
			t.Fatalf("MarkTOTPStep(pending) = (%v, %v); want (false, nil)", ok, err)
		}
		// Unknown tenant likewise.
		if ok, err := s.MarkTOTPStep("t-nobody", 10); err != nil || ok {
			t.Fatalf("MarkTOTPStep(unknown) = (%v, %v); want (false, nil)", ok, err)
		}
	})

	t.Run("RecoveryCodeSingleUse", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-6", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		h := hashRecoveryCode("recover-me")
		if err := s.Activate("t-6", 1, []string{h, hashRecoveryCode("other")}); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		// First consume succeeds.
		if ok, err := s.ConsumeRecoveryCode("t-6", h); err != nil || !ok {
			t.Fatalf("ConsumeRecoveryCode(first) = (%v, %v); want (true, nil)", ok, err)
		}
		// Second consume of the same hash fails (single-use).
		if ok, err := s.ConsumeRecoveryCode("t-6", h); err != nil || ok {
			t.Fatalf("ConsumeRecoveryCode(reuse) = (%v, %v); want (false, nil)", ok, err)
		}
		// One of the two issued codes remains.
		rec, _, _ := s.GetMFA("t-6")
		if rec.RecoveryRemaining != 1 {
			t.Fatalf("RecoveryRemaining = %d, want 1", rec.RecoveryRemaining)
		}
	})

	t.Run("ConsumeUnknownRecoveryCode", func(t *testing.T) {
		s := newStore(t)
		mustEnrollAndActivate(t, s, "t-7", "SECRET", 1)
		if ok, err := s.ConsumeRecoveryCode("t-7", "nonexistent-hash"); err != nil || ok {
			t.Fatalf("ConsumeRecoveryCode(unknown) = (%v, %v); want (false, nil)", ok, err)
		}
		if ok, err := s.ConsumeRecoveryCode("t-7", ""); err != nil || ok {
			t.Fatalf("ConsumeRecoveryCode(empty) = (%v, %v); want (false, nil)", ok, err)
		}
	})

	t.Run("DisableRemovesEverything", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-8", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		if err := s.Activate("t-8", 1, []string{hashRecoveryCode("a")}); err != nil {
			t.Fatalf("Activate: %v", err)
		}
		if err := s.Disable("t-8"); err != nil {
			t.Fatalf("Disable: %v", err)
		}
		if _, ok, err := s.GetMFA("t-8"); err != nil || ok {
			t.Fatalf("GetMFA after Disable = (_, %v, %v); want (_, false, nil)", ok, err)
		}
		// Disabling again is a no-op, not an error.
		if err := s.Disable("t-8"); err != nil {
			t.Fatalf("Disable(idempotent): %v", err)
		}
		// A fresh enrollment after disable is allowed (not blocked by
		// the prior active state) and starts pending.
		if err := s.BeginEnrollment("t-8", "NEWSECRET"); err != nil {
			t.Fatalf("re-enroll after disable: %v", err)
		}
		rec, _, _ := s.GetMFA("t-8")
		if rec.Active || rec.Secret != "NEWSECRET" {
			t.Fatalf("re-enroll record = %+v; want pending NEWSECRET", rec)
		}
	})

	t.Run("DisablePendingClearsOnlyPending", func(t *testing.T) {
		s := newStore(t)
		// No enrollment: nothing to clear, reported cleared=false.
		if cleared, err := s.DisablePending("t-dp-none"); err != nil || cleared {
			t.Fatalf("DisablePending(missing) = (%v, %v); want (false, nil)", cleared, err)
		}
		// Pending enrollment: cleared and removed.
		if err := s.BeginEnrollment("t-dp", "SECRET"); err != nil {
			t.Fatalf("BeginEnrollment: %v", err)
		}
		if cleared, err := s.DisablePending("t-dp"); err != nil || !cleared {
			t.Fatalf("DisablePending(pending) = (%v, %v); want (true, nil)", cleared, err)
		}
		if _, ok, err := s.GetMFA("t-dp"); err != nil || ok {
			t.Fatalf("GetMFA after DisablePending = (_, %v, %v); want (_, false, nil)", ok, err)
		}
	})

	t.Run("DisablePendingRefusesActive", func(t *testing.T) {
		s := newStore(t)
		// This is the TOCTOU guard: an active enrollment must NOT be
		// removed by the no-second-factor pending-clear path.
		mustEnrollAndActivate(t, s, "t-dp-active", "SECRET", 42)
		cleared, err := s.DisablePending("t-dp-active")
		if err != nil {
			t.Fatalf("DisablePending(active) err = %v; want nil", err)
		}
		if cleared {
			t.Fatal("DisablePending must NOT clear an active enrollment")
		}
		// The active enrollment (secret + watermark) must survive intact.
		rec, ok, err := s.GetMFA("t-dp-active")
		if err != nil || !ok {
			t.Fatalf("GetMFA after refused DisablePending = (_, %v, %v)", ok, err)
		}
		if !rec.Active || rec.Secret != "SECRET" || rec.LastStep != 42 {
			t.Fatalf("active record altered by DisablePending: %+v", rec)
		}
	})

	t.Run("ReEnrollPendingReplacesSecret", func(t *testing.T) {
		s := newStore(t)
		if err := s.BeginEnrollment("t-9", "FIRST"); err != nil {
			t.Fatalf("BeginEnrollment(first): %v", err)
		}
		// Re-enrolling while still pending swaps in the new secret
		// (the user re-scanned a fresh QR before confirming).
		if err := s.BeginEnrollment("t-9", "SECOND"); err != nil {
			t.Fatalf("BeginEnrollment(second): %v", err)
		}
		rec, _, _ := s.GetMFA("t-9")
		if rec.Secret != "SECOND" || rec.Active {
			t.Fatalf("re-enroll pending record = %+v; want pending SECOND", rec)
		}
	})
}

// mustEnrollAndActivate is a test helper that drives a tenant to the
// active MFA state with a single recovery code.
func mustEnrollAndActivate(t *testing.T, s MFAStore, tenantID, secret string, firstStep int64) {
	t.Helper()
	if err := s.BeginEnrollment(tenantID, secret); err != nil {
		t.Fatalf("BeginEnrollment(%s): %v", tenantID, err)
	}
	if err := s.Activate(tenantID, firstStep, []string{hashRecoveryCode("seed")}); err != nil {
		t.Fatalf("Activate(%s): %v", tenantID, err)
	}
}
