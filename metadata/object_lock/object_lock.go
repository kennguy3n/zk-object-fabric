// Package object_lock defines the provider-neutral domain types for
// S3 Object Lock / WORM (WS8.3): the bucket-level lock configuration
// (default retention rule) and the per-object retention and legal-hold
// values. It carries no persistence of its own — bucket-level Config
// is stored through metadata/bucket_config (the per-bucket S3 config
// store), and per-object Retention / LegalHold live on the object
// manifest (metadata.ObjectManifest) so they version with the object.
//
// Object Lock can only protect data that cannot be silently
// overwritten, so it requires bucket versioning to be enabled
// (dependency on WS8.4). The api/s3compat layer enforces that
// dependency and maps between these types and the manifest's flat
// retention/legal-hold fields; this package only owns the value
// semantics (valid modes, default-rule consistency, retain-until
// computation).
package object_lock

import (
	"errors"
	"time"
)

// RetentionMode is the S3 Object Lock retention mode applied to a
// protected object version.
type RetentionMode string

const (
	// ModeGovernance protects a version from deletion/overwrite, but
	// a caller with the s3:BypassGovernanceRetention permission and
	// the x-amz-bypass-governance-retention:true header may shorten
	// or remove the retention and delete the version.
	ModeGovernance RetentionMode = "GOVERNANCE"
	// ModeCompliance protects a version absolutely: nobody (not even
	// the root account) may shorten the retention or delete the
	// version until RetainUntilDate passes.
	ModeCompliance RetentionMode = "COMPLIANCE"
)

// Valid reports whether m is a settable retention mode.
func (m RetentionMode) Valid() bool {
	switch m {
	case ModeGovernance, ModeCompliance:
		return true
	default:
		return false
	}
}

// LegalHoldStatus is the S3 Object Lock legal-hold flag for an object
// version. A legal hold blocks deletion independently of retention and
// has no expiry — it stays until explicitly turned OFF.
type LegalHoldStatus string

const (
	// LegalHoldOff means no legal hold is in force.
	LegalHoldOff LegalHoldStatus = "OFF"
	// LegalHoldOn means the version is held and cannot be deleted
	// until the hold is turned OFF, regardless of retention.
	LegalHoldOn LegalHoldStatus = "ON"
)

// Valid reports whether s is a settable legal-hold status.
func (s LegalHoldStatus) Valid() bool {
	return s == LegalHoldOff || s == LegalHoldOn
}

// Config is the bucket-level Object Lock configuration set by
// PutObjectLockConfiguration. When Enabled is false the bucket has no
// Object Lock and the other fields are ignored. When Enabled is true
// the bucket MUST have versioning enabled (enforced at the API layer).
//
// A default retention rule is optional: when DefaultMode is empty the
// bucket has Object Lock turned on but applies no automatic retention
// to new objects (callers set retention per object). When DefaultMode
// is set, exactly one of DefaultDays / DefaultYears must be > 0; new
// object versions inherit that retention at PUT time.
type Config struct {
	Enabled      bool
	DefaultMode  RetentionMode
	DefaultDays  int
	DefaultYears int
}

// Valid checks internal consistency of a bucket Object Lock config.
func (c Config) Valid() error {
	if !c.Enabled {
		// A disabled config carries no rule. Reject stray default
		// fields so a malformed request can't be silently stored as
		// "disabled but with a rule".
		if c.DefaultMode != "" || c.DefaultDays != 0 || c.DefaultYears != 0 {
			return errors.New("object_lock: default retention set on a disabled configuration")
		}
		return nil
	}
	hasRule := c.DefaultMode != "" || c.DefaultDays != 0 || c.DefaultYears != 0
	if !hasRule {
		return nil // Object Lock enabled with no default rule.
	}
	if !c.DefaultMode.Valid() {
		return errors.New("object_lock: default retention mode must be GOVERNANCE or COMPLIANCE")
	}
	if (c.DefaultDays > 0) == (c.DefaultYears > 0) {
		return errors.New("object_lock: default retention requires exactly one of Days or Years")
	}
	if c.DefaultDays < 0 || c.DefaultYears < 0 {
		return errors.New("object_lock: default retention period must be positive")
	}
	return nil
}

// HasDefaultRetention reports whether the config carries an automatic
// default retention rule that new object versions should inherit.
func (c Config) HasDefaultRetention() bool {
	return c.Enabled && c.DefaultMode.Valid() && (c.DefaultDays > 0 || c.DefaultYears > 0)
}

// DefaultRetainUntil computes the retain-until date a new object
// version inherits from the bucket default rule, relative to now.
// Callers should only use it when HasDefaultRetention is true. AWS
// treats a year as 365 days for Object Lock default retention.
func (c Config) DefaultRetainUntil(now time.Time) time.Time {
	if c.DefaultDays > 0 {
		return now.AddDate(0, 0, c.DefaultDays)
	}
	return now.AddDate(0, 0, c.DefaultYears*365)
}

// Retention is a per-object-version retention setting (the body of
// PutObjectRetention / GetObjectRetention). A zero RetainUntil with an
// empty Mode means the version has no retention.
type Retention struct {
	Mode        RetentionMode
	RetainUntil time.Time
}

// Active reports whether the retention is still in force at now.
func (r Retention) Active(now time.Time) bool {
	return r.Mode.Valid() && r.RetainUntil.After(now)
}

// Valid checks a per-object retention payload.
func (r Retention) Valid() error {
	if !r.Mode.Valid() {
		return errors.New("object_lock: retention mode must be GOVERNANCE or COMPLIANCE")
	}
	if r.RetainUntil.IsZero() {
		return errors.New("object_lock: retention requires a RetainUntilDate")
	}
	return nil
}
