package rlsdb

import (
	"strings"
	"testing"
)

func TestStatements_Validation(t *testing.T) {
	if _, err := Statements("bad-table", "zkof_app"); err == nil {
		t.Error("Statements accepted an unsafe table identifier")
	}
	if _, err := Statements("content_index", "zkof_app; DROP TABLE x"); err == nil {
		t.Error("Statements accepted an unsafe role identifier")
	}
	if _, err := Statements("9bad", "zkof_app"); err == nil {
		t.Error("Statements accepted a table identifier starting with a digit")
	}

	stmts, err := Statements("content_index", "zkof_app")
	if err != nil {
		t.Fatalf("Statements(valid): %v", err)
	}
	joined := strings.Join(stmts, "\n")
	for _, want := range []string{
		"ENABLE ROW LEVEL SECURITY",
		"FORCE ROW LEVEL SECURITY",
		"CREATE POLICY tenant_isolation",
		"current_setting('zkof.tenant_id', true)",
		"current_setting('zkof.scan_all', true)",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON content_index TO zkof_app",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("Statements output missing %q\n--- got ---\n%s", want, joined)
		}
	}

	// WITH CHECK must NOT honour the scan_all bypass: even a global sweep
	// may not write a row under a foreign tenant_id. Inspect only the text
	// of the WITH CHECK clause (scan_all legitimately appears in the USING
	// clause of the same statement).
	for _, s := range stmts {
		idx := strings.Index(s, "WITH CHECK")
		if idx < 0 {
			continue
		}
		if strings.Contains(s[idx:], GUCScanAll) {
			t.Errorf("WITH CHECK clause must not reference %s (write bypass):\n%s", GUCScanAll, s[idx:])
		}
	}
}
