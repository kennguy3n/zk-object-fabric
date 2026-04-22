// Package migration defines the cloud→local migration state machine
// used by ZK Object Fabric. See docs/PROPOSAL.md §4.3.
//
// A manifest's migration phase advances through a fixed sequence. The
// state is recorded on the manifest itself so that readers can decide
// whether to look at Wasabi, the local cell, or both, and so that the
// background rebalancer and drain workers can resume safely.
package migration

import "fmt"

// MigrationPhase is one step in the cloud→local migration lifecycle.
type MigrationPhase string

const (
	// WasabiPrimary: object lives only on Wasabi. This is the Phase 1
	// default.
	WasabiPrimary MigrationPhase = "wasabi_primary"

	// DualWrite: new writes go to both Wasabi and the local cell.
	// Reads prefer Wasabi until the local copy is confirmed durable.
	DualWrite MigrationPhase = "dual_write"

	// LocalPrimaryWasabiBackup: local cell is authoritative for reads
	// and writes; Wasabi retains a copy for resilience.
	LocalPrimaryWasabiBackup MigrationPhase = "local_primary_wasabi_backup"

	// LocalPrimaryWasabiDrain: local cell is authoritative; a drain
	// worker is deleting the Wasabi copy according to the grace
	// period.
	LocalPrimaryWasabiDrain MigrationPhase = "local_primary_wasabi_drain"

	// LocalOnly: object lives only on the local cell. Wasabi no longer
	// holds a copy.
	LocalOnly MigrationPhase = "local_only"
)

// Valid reports whether p is a recognised phase.
func (p MigrationPhase) Valid() bool {
	switch p {
	case WasabiPrimary, DualWrite, LocalPrimaryWasabiBackup,
		LocalPrimaryWasabiDrain, LocalOnly:
		return true
	default:
		return false
	}
}

// allowedTransitions encodes the legal forward and rollback edges of
// the migration state machine. A manifest may advance one step at a
// time, or roll back one step during a bad cut-over.
var allowedTransitions = map[MigrationPhase]map[MigrationPhase]bool{
	WasabiPrimary: {
		DualWrite: true,
	},
	DualWrite: {
		LocalPrimaryWasabiBackup: true,
		WasabiPrimary:            true, // rollback
	},
	LocalPrimaryWasabiBackup: {
		LocalPrimaryWasabiDrain: true,
		DualWrite:               true, // rollback
	},
	LocalPrimaryWasabiDrain: {
		LocalOnly:                true,
		LocalPrimaryWasabiBackup: true, // rollback
	},
	LocalOnly: {},
}

// CanTransition reports whether from → to is a legal single-step
// transition.
func CanTransition(from, to MigrationPhase) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	if from == to {
		return false
	}
	return allowedTransitions[from][to]
}

// ValidateTransition returns nil if from → to is legal and a
// descriptive error otherwise.
func ValidateTransition(from, to MigrationPhase) error {
	if !from.Valid() {
		return fmt.Errorf("migration: invalid source phase %q", from)
	}
	if !to.Valid() {
		return fmt.Errorf("migration: invalid target phase %q", to)
	}
	if from == to {
		return fmt.Errorf("migration: no-op transition from %q to itself", from)
	}
	if !allowedTransitions[from][to] {
		return fmt.Errorf("migration: illegal transition %q → %q", from, to)
	}
	return nil
}
