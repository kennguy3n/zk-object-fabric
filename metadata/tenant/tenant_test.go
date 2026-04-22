package tenant

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func sampleTenant() *Tenant {
	return &Tenant{
		ID:           "t_01H0000000000000000000000",
		Name:         "acme-fintech",
		ContractType: ContractB2BDedicated,
		LicenseTier:  LicenseStandard,
		Keys: Keys{
			RootKeyRef: "cmk://acme/prod/root",
			DEKPolicy:  DEKPerObject,
		},
		PlacementDefault: PlacementDefault{
			PolicyRef: "p_country_strict",
		},
		Budgets: Budgets{
			EgressTBMonth:  50,
			RequestsPerSec: 5000,
		},
		Abuse: AbuseConfig{
			AnomalyProfile: "finance",
			CDNShielding:   "enabled",
		},
		Billing: Billing{
			Currency:     "USD",
			InvoiceGroup: "acme-corp",
		},
	}
}

func TestTenant_Validate_OK(t *testing.T) {
	if err := sampleTenant().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTenant_Validate_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Tenant)
		wantSub string
	}{
		{"missing id", func(t *Tenant) { t.ID = "" }, "id is required"},
		{"missing name", func(t *Tenant) { t.Name = "" }, "name is required"},
		{"bad contract", func(t *Tenant) { t.ContractType = "nope" }, "contract_type"},
		{"bad tier", func(t *Tenant) { t.LicenseTier = "nope" }, "license_tier"},
		{"missing key ref", func(t *Tenant) { t.Keys.RootKeyRef = "" }, "root_key_ref is required"},
		{"bad dek policy", func(t *Tenant) { t.Keys.DEKPolicy = "nope" }, "dek_policy"},
		{"missing policy ref", func(t *Tenant) { t.PlacementDefault.PolicyRef = "" }, "policy_ref is required"},
		{"negative egress", func(t *Tenant) { t.Budgets.EgressTBMonth = -1 }, "egress_tb_month"},
		{"negative rps", func(t *Tenant) { t.Budgets.RequestsPerSec = -1 }, "requests_per_sec"},
		{"missing currency", func(t *Tenant) { t.Billing.Currency = "" }, "currency is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ten := sampleTenant()
			tc.mutate(ten)
			err := ten.Validate()
			if err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Validate error = %q, want substring %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestTenant_JSONRoundTrip(t *testing.T) {
	orig := sampleTenant()
	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Tenant
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	data2, err := json.Marshal(&got)
	if err != nil {
		t.Fatalf("Marshal round-trip: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("JSON not byte-identical after round-trip:\nfirst:  %s\nsecond: %s", data, data2)
	}
}

func TestTenant_YAMLRoundTrip(t *testing.T) {
	orig := sampleTenant()
	data, err := yaml.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got Tenant
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	data2, err := yaml.Marshal(&got)
	if err != nil {
		t.Fatalf("Marshal round-trip: %v", err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("YAML not byte-identical after round-trip:\nfirst:\n%s\nsecond:\n%s", data, data2)
	}
}

func TestTenant_YAMLShape(t *testing.T) {
	// docs/PROPOSAL.md §5.5 specifies the YAML keys operators will
	// hand-edit. Verify they appear verbatim in a marshalled tenant
	// so the doc and the code do not drift.
	data, err := yaml.Marshal(sampleTenant())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body := string(data)
	for _, key := range []string{
		"id:", "name:", "contract_type:", "license_tier:",
		"keys:", "root_key_ref:", "dek_policy:",
		"placement_default:", "policy_ref:",
		"budgets:", "egress_tb_month:", "requests_per_sec:",
		"abuse:", "anomaly_profile:", "cdn_shielding:",
		"billing:", "currency:", "invoice_group:",
	} {
		if !strings.Contains(body, key) {
			t.Errorf("YAML body missing %q; got:\n%s", key, body)
		}
	}
}
