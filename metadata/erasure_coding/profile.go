// Package erasure_coding defines the erasure-coding profile types used
// by the Phase 2+ local-DC data plane. See docs/PROPOSAL.md §3.8 and
// §6 for the context in which these profiles are consumed.
//
// Phase 1 does not perform erasure coding: objects land on Wasabi,
// which provides its own durability. The profiles defined here are
// the declarative specification that Phase 2 engineers will wire into
// the repair engine, the placement engine, and the cell manifest.
package erasure_coding

import (
	"fmt"
)

// ErasureCodingProfile is the declarative description of a Reed-
// Solomon style (k + m) erasure-coding scheme.
//
// A profile with DataShards=k and ParityShards=m can tolerate the
// loss of any m shards out of the (k + m) written. StripeSize is the
// number of plaintext (pre-encryption) bytes that each stripe
// represents on the wire before shards are generated.
type ErasureCodingProfile struct {
	// Name is a short human-readable label (e.g. "6+2", "8+3", "10+4")
	// that operators can reference in placement policy and logs.
	Name string `json:"name" yaml:"name"`

	// DataShards is the number of data shards per stripe (the "k" in
	// k + m). Must be positive.
	DataShards int `json:"data_shards" yaml:"data_shards"`

	// ParityShards is the number of parity shards per stripe (the
	// "m" in k + m). Must be positive.
	ParityShards int `json:"parity_shards" yaml:"parity_shards"`

	// StripeSize is the size in bytes of one stripe's worth of
	// plaintext before it is split into shards. Typical values are
	// powers of two between 1 MiB and 16 MiB.
	StripeSize int64 `json:"stripe_size" yaml:"stripe_size"`
}

// Standard Phase 2+ profiles. Phase 1 declares them so the schema is
// stable; Phase 2 wires them into the placement engine.
var (
	// Profile6Plus2 is the entry-level profile. Storage overhead is
	// 1.33x with tolerance for 2 shard losses per stripe.
	Profile6Plus2 = ErasureCodingProfile{
		Name:         "6+2",
		DataShards:   6,
		ParityShards: 2,
		StripeSize:   4 * 1024 * 1024, // 4 MiB
	}

	// Profile8Plus3 is the default production profile. Storage
	// overhead is 1.375x with tolerance for 3 shard losses per
	// stripe. This matches the erasure-overhead target in
	// docs/PROGRESS.md line 208.
	Profile8Plus3 = ErasureCodingProfile{
		Name:         "8+3",
		DataShards:   8,
		ParityShards: 3,
		StripeSize:   4 * 1024 * 1024, // 4 MiB
	}

	// Profile10Plus4 is the high-durability profile for sovereign and
	// long-tail archival workloads. Storage overhead is 1.4x with
	// tolerance for 4 shard losses per stripe.
	Profile10Plus4 = ErasureCodingProfile{
		Name:         "10+4",
		DataShards:   10,
		ParityShards: 4,
		StripeSize:   4 * 1024 * 1024, // 4 MiB
	}

	// Profile12Plus4 is a wide profile for large cells where
	// per-stripe overhead matters more than single-node recovery
	// latency. Storage overhead is 1.333x with tolerance for 4
	// shard losses per stripe.
	Profile12Plus4 = ErasureCodingProfile{
		Name:         "12+4",
		DataShards:   12,
		ParityShards: 4,
		StripeSize:   4 * 1024 * 1024, // 4 MiB
	}

	// Profile16Plus4 is the widest default profile. It minimises
	// raw-overhead multiplier (1.25x) at the cost of more IO fan-out
	// per stripe. Intended for cold / archival B2B cells where
	// storage COGS dominates.
	Profile16Plus4 = ErasureCodingProfile{
		Name:         "16+4",
		DataShards:   16,
		ParityShards: 4,
		StripeSize:   4 * 1024 * 1024, // 4 MiB
	}
)

// StandardProfiles returns the Phase 2+ profiles in declaration
// order. The slice is freshly allocated so callers may mutate it.
func StandardProfiles() []ErasureCodingProfile {
	return []ErasureCodingProfile{
		Profile6Plus2,
		Profile8Plus3,
		Profile10Plus4,
		Profile12Plus4,
		Profile16Plus4,
	}
}

// TotalShards returns DataShards + ParityShards.
func (p ErasureCodingProfile) TotalShards() int {
	return p.DataShards + p.ParityShards
}

// StorageOverhead returns the ratio of total bytes written to
// plaintext bytes for one stripe, e.g. 1.375 for 8+3.
func (p ErasureCodingProfile) StorageOverhead() float64 {
	if p.DataShards == 0 {
		return 0
	}
	return float64(p.DataShards+p.ParityShards) / float64(p.DataShards)
}

// Validate performs structural checks on the profile.
//
// It enforces the invariants needed to safely wire a profile into the
// Phase 2 repair engine:
//
//   - Name is required.
//   - DataShards and ParityShards are both positive. Pure replication
//     (m = 0) is not an erasure-coding profile; Phase 1 uses Wasabi
//     durability for that case.
//   - StripeSize is positive. Cells may enforce a stricter minimum
//     but zero or negative values are always illegal.
//
// StripeSize is NOT required to be divisible by DataShards: real
// Reed-Solomon codecs pad the final sub-stripe, and the standard
// profiles (6+2, 8+3, 10+4) share a 4 MiB stripe that is not
// divisible by 6 or 10.
func (p ErasureCodingProfile) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("erasure_coding: name is required")
	}
	if p.DataShards <= 0 {
		return fmt.Errorf("erasure_coding: data_shards must be > 0 (got %d)", p.DataShards)
	}
	if p.ParityShards <= 0 {
		return fmt.Errorf("erasure_coding: parity_shards must be > 0 (got %d)", p.ParityShards)
	}
	if p.StripeSize <= 0 {
		return fmt.Errorf("erasure_coding: stripe_size must be > 0 (got %d)", p.StripeSize)
	}
	return nil
}
