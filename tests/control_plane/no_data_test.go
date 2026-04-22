// Package control_plane_test is a contract test that asserts no
// customer object bytes cross the AWS boundary.
//
// The promise in docs/PROPOSAL.md §2.2 and §3.1 is that the AWS
// control plane only ever sees opaque metadata: manifest IDs, sizes,
// hashes, counters, billing events, and encrypted manifest bodies —
// never raw object payloads. This test reflects over every type
// reachable from the control-plane packages and rejects any field
// that could carry a plaintext or ciphertext object body.
//
// The rule is simple and conservative: the control-plane API surface
// must not contain fields typed io.Reader, io.ReadCloser, io.Writer,
// []byte, or *bytes.Buffer. Manifest bodies, which are encrypted,
// live behind the ManifestStore interface and flow as *ObjectManifest
// values whose Go representation is JSON-serializable metadata, not
// opaque bytes.
package control_plane_test

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/billing"
	"github.com/kennguy3n/zk-object-fabric/metadata"
	"github.com/kennguy3n/zk-object-fabric/metadata/manifest_store"
	"github.com/kennguy3n/zk-object-fabric/metadata/placement_policy"
	"github.com/kennguy3n/zk-object-fabric/metadata/tenant"
)

// controlPlaneType names the in-package types that live on the AWS
// side of the boundary. Adding a new type to the AWS control plane
// without adding it here should be treated as a review-time error.
type controlPlaneType struct {
	name   string
	sample any
}

func controlPlaneTypes() []controlPlaneType {
	return []controlPlaneType{
		// metadata — manifest shape and store contract.
		{"metadata.ObjectManifest", metadata.ObjectManifest{}},
		{"metadata.EncryptionConfig", metadata.EncryptionConfig{}},
		{"metadata.Piece", metadata.Piece{}},
		{"metadata.MigrationState", metadata.MigrationState{}},
		{"metadata.PlacementPolicy", metadata.PlacementPolicy{}},
		{"manifest_store.ManifestKey", manifest_store.ManifestKey{}},
		{"manifest_store.ListResult", manifest_store.ListResult{}},

		// tenant — multi-tenancy configuration.
		{"tenant.Tenant", tenant.Tenant{}},
		{"tenant.Keys", tenant.Keys{}},
		{"tenant.PlacementDefault", tenant.PlacementDefault{}},
		{"tenant.Budgets", tenant.Budgets{}},
		{"tenant.AbuseConfig", tenant.AbuseConfig{}},
		{"tenant.Billing", tenant.Billing{}},

		// placement_policy — DSL carried in the control plane.
		{"placement_policy.Policy", placement_policy.Policy{}},
		{"placement_policy.PolicySpec", placement_policy.PolicySpec{}},
		{"placement_policy.EncryptionSpec", placement_policy.EncryptionSpec{}},
		{"placement_policy.PlacementSpec", placement_policy.PlacementSpec{}},

		// billing — per-tenant usage counters and budget policy.
		{"billing.Counter", billing.Counter{}},
		{"billing.UsageEvent", billing.UsageEvent{}},
		{"billing.BudgetPolicy", billing.BudgetPolicy{}},
	}
}

// rawBytesTypes enumerates the Go types that could hold a raw object
// body. A control-plane field of any of these types violates the
// zero-customer-data invariant.
func rawBytesTypes() []reflect.Type {
	return []reflect.Type{
		reflect.TypeOf((*io.Reader)(nil)).Elem(),
		reflect.TypeOf((*io.ReadCloser)(nil)).Elem(),
		reflect.TypeOf((*io.Writer)(nil)).Elem(),
		reflect.TypeOf([]byte(nil)),
		reflect.TypeOf((*bytes.Buffer)(nil)),
	}
}

// fieldPathFor walks a struct recursively and reports any field
// whose type matches one of rawBytesTypes. It ignores field types
// from the standard library time/json packages (time.Time carries
// unexported byte slices that are not customer data).
func fieldPathFor(t reflect.Type, prefix string, visited map[reflect.Type]bool) []string {
	if t == nil || visited[t] {
		return nil
	}
	visited[t] = true

	// Look through pointers and named types to the underlying kind.
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.PkgPath() == "time" {
		// time.Time intentionally elided.
		return nil
	}

	var violations []string
	switch t.Kind() {
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			path := prefix + "." + f.Name
			if violated, why := fieldViolates(f.Type); violated {
				violations = append(violations, path+": "+why)
				continue
			}
			violations = append(violations, fieldPathFor(f.Type, path, visited)...)
		}
	case reflect.Slice, reflect.Array:
		elem := t.Elem()
		// []byte is a raw-body type and is handled by fieldViolates
		// on the parent struct field. Here we only recurse into the
		// element type for struct slices.
		if elem.Kind() == reflect.Struct {
			violations = append(violations, fieldPathFor(elem, prefix+"[*]", visited)...)
		}
	case reflect.Map:
		elem := t.Elem()
		if elem.Kind() == reflect.Struct {
			violations = append(violations, fieldPathFor(elem, prefix+"[*]", visited)...)
		}
	}
	return violations
}

// fieldViolates reports whether a field of type ft is a raw-body type.
func fieldViolates(ft reflect.Type) (bool, string) {
	for _, banned := range rawBytesTypes() {
		if banned.Kind() == reflect.Interface {
			if ft.Kind() == reflect.Interface && ft.Implements(banned) {
				return true, "field is " + ft.String() + " (implements " + banned.String() + ")"
			}
			if ft.Kind() != reflect.Interface && reflect.PtrTo(ft).Implements(banned) {
				return true, "field is " + ft.String() + " (satisfies " + banned.String() + ")"
			}
		} else if ft == banned {
			return true, "field is " + ft.String()
		}
	}
	return false, ""
}

func TestControlPlane_NoRawObjectBytes(t *testing.T) {
	for _, ct := range controlPlaneTypes() {
		t.Run(ct.name, func(t *testing.T) {
			visited := map[reflect.Type]bool{}
			viol := fieldPathFor(reflect.TypeOf(ct.sample), ct.name, visited)
			if len(viol) != 0 {
				t.Fatalf("control-plane type %s must not carry raw object bytes; found:\n  %s",
					ct.name, strings.Join(viol, "\n  "))
			}
		})
	}
}

// TestControlPlane_ManifestStoreContract asserts that the
// ManifestStore interface only accepts and returns manifest-shaped
// values. Any ManifestStore method that takes or returns io.Reader,
// io.ReadCloser, or []byte would break the zero-customer-data
// invariant at compile time.
func TestControlPlane_ManifestStoreContract(t *testing.T) {
	storeType := reflect.TypeOf((*manifest_store.ManifestStore)(nil)).Elem()
	for i := 0; i < storeType.NumMethod(); i++ {
		m := storeType.Method(i)
		for j := 0; j < m.Type.NumIn(); j++ {
			checkBannedParam(t, "ManifestStore."+m.Name+".In["+itoa(j)+"]", m.Type.In(j))
		}
		for j := 0; j < m.Type.NumOut(); j++ {
			checkBannedParam(t, "ManifestStore."+m.Name+".Out["+itoa(j)+"]", m.Type.Out(j))
		}
	}
}

func checkBannedParam(t *testing.T, where string, p reflect.Type) {
	t.Helper()
	if violated, why := fieldViolates(p); violated {
		t.Fatalf("%s must not carry raw object bytes: %s", where, why)
	}
	// Also walk into pointer/struct returns so e.g. *Manifest can't
	// grow a []byte body later without tripping this test.
	if p.Kind() == reflect.Ptr {
		p = p.Elem()
	}
	if p.Kind() == reflect.Struct {
		visited := map[reflect.Type]bool{}
		viol := fieldPathFor(p, where, visited)
		if len(viol) != 0 {
			t.Fatalf("%s must not carry raw object bytes; found:\n  %s", where, strings.Join(viol, "\n  "))
		}
	}
}

// itoa is a tiny strconv.Itoa avoiding an extra import in a test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		buf[n] = '-'
	}
	return string(buf[n:])
}

// TestControlPlane_BillingCountersAreOpaque is a positive assertion:
// the billing counters must only carry IDs, names, and numeric usage
// fields. The test is a companion to TestControlPlane_NoRawObjectBytes
// and documents the intent for future reviewers.
func TestControlPlane_BillingCountersAreOpaque(t *testing.T) {
	cases := []any{
		billing.Counter{},
		billing.UsageEvent{},
		billing.BudgetPolicy{},
	}
	allowedKinds := map[reflect.Kind]bool{
		reflect.String:  true,
		reflect.Int:     true,
		reflect.Int64:   true,
		reflect.Uint64:  true,
		reflect.Float64: true,
		reflect.Struct:  true, // time.Time
	}
	for _, c := range cases {
		rt := reflect.TypeOf(c)
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !allowedKinds[f.Type.Kind()] {
				t.Errorf("%s.%s: kind %s is not in the allowed opaque-metadata set", rt.Name(), f.Name, f.Type.Kind())
			}
		}
	}
}
