package sqlite

import (
	"errors"
	"strconv"

	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
)

func validateKey(key manifest_store.ManifestKey) error {
	if key.TenantID == "" {
		return errors.New("sqlite: tenant_id is required")
	}
	if key.Bucket == "" {
		return errors.New("sqlite: bucket is required")
	}
	if key.ObjectKeyHash == "" {
		return errors.New("sqlite: object_key_hash is required")
	}
	if key.VersionID == "" {
		return errors.New("sqlite: version_id is required")
	}
	return nil
}

// isSafeIdent validates that s is a plausible SQL identifier: ASCII
// letters, digits, and underscore only. This keeps the table name
// safe for fmt.Sprintf interpolation without a full quoting routine.
func isSafeIdent(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		switch {
		case isLetter:
		case isDigit && i > 0:
		default:
			return false
		}
	}
	return true
}

func parseCursor(c string) (int64, error) {
	n, err := strconv.ParseInt(c, 10, 64)
	if err != nil {
		return 0, errors.New("sqlite: invalid list cursor")
	}
	return n, nil
}

func formatCursor(seq int64) string {
	return strconv.FormatInt(seq, 10)
}
