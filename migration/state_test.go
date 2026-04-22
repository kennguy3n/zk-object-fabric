package migration

import "testing"

func TestMigrationPhase_Valid(t *testing.T) {
	for _, p := range []MigrationPhase{
		WasabiPrimary, DualWrite, LocalPrimaryWasabiBackup,
		LocalPrimaryWasabiDrain, LocalOnly,
	} {
		if !p.Valid() {
			t.Errorf("MigrationPhase(%q).Valid() = false, want true", p)
		}
	}
	if MigrationPhase("bogus").Valid() {
		t.Errorf("MigrationPhase(\"bogus\").Valid() = true, want false")
	}
}

func TestCanTransition_ForwardPath(t *testing.T) {
	forward := []struct {
		from, to MigrationPhase
	}{
		{WasabiPrimary, DualWrite},
		{DualWrite, LocalPrimaryWasabiBackup},
		{LocalPrimaryWasabiBackup, LocalPrimaryWasabiDrain},
		{LocalPrimaryWasabiDrain, LocalOnly},
	}
	for _, tc := range forward {
		if !CanTransition(tc.from, tc.to) {
			t.Errorf("CanTransition(%q, %q) = false, want true", tc.from, tc.to)
		}
	}
}

func TestCanTransition_Rollback(t *testing.T) {
	rollbacks := []struct {
		from, to MigrationPhase
	}{
		{DualWrite, WasabiPrimary},
		{LocalPrimaryWasabiBackup, DualWrite},
		{LocalPrimaryWasabiDrain, LocalPrimaryWasabiBackup},
	}
	for _, tc := range rollbacks {
		if !CanTransition(tc.from, tc.to) {
			t.Errorf("CanTransition rollback %q → %q = false, want true", tc.from, tc.to)
		}
	}
}

func TestCanTransition_Illegal(t *testing.T) {
	illegal := []struct {
		from, to MigrationPhase
	}{
		{WasabiPrimary, LocalOnly},                // skip
		{WasabiPrimary, LocalPrimaryWasabiBackup}, // skip
		{LocalOnly, DualWrite},                    // terminal rollback
		{LocalOnly, WasabiPrimary},
		{WasabiPrimary, WasabiPrimary},       // no-op
		{MigrationPhase("x"), WasabiPrimary}, // invalid source
		{WasabiPrimary, MigrationPhase("x")}, // invalid target
	}
	for _, tc := range illegal {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("CanTransition(%q, %q) = true, want false", tc.from, tc.to)
		}
	}
}

func TestValidateTransition_ErrorMessages(t *testing.T) {
	if err := ValidateTransition(WasabiPrimary, DualWrite); err != nil {
		t.Fatalf("ValidateTransition happy path: %v", err)
	}
	if err := ValidateTransition(WasabiPrimary, WasabiPrimary); err == nil {
		t.Fatal("ValidateTransition no-op: want error, got nil")
	}
	if err := ValidateTransition(WasabiPrimary, LocalOnly); err == nil {
		t.Fatal("ValidateTransition skip: want error, got nil")
	}
	if err := ValidateTransition(MigrationPhase("bogus"), DualWrite); err == nil {
		t.Fatal("ValidateTransition invalid source: want error, got nil")
	}
}

func TestLocalOnly_IsTerminal(t *testing.T) {
	for _, p := range []MigrationPhase{
		WasabiPrimary, DualWrite, LocalPrimaryWasabiBackup,
		LocalPrimaryWasabiDrain,
	} {
		if CanTransition(LocalOnly, p) {
			t.Errorf("CanTransition(LocalOnly, %q) = true, want false (LocalOnly is terminal)", p)
		}
	}
}
