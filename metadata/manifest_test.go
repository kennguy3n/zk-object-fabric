package metadata

import (
	"bytes"
	"encoding/json"
	"testing"
)

func sampleManifest() *ObjectManifest {
	return &ObjectManifest{
		TenantID:      "tnt_123",
		Bucket:        "prod-assets",
		ObjectKeyHash: "blake3:deadbeef",
		VersionID:     "v7",
		ObjectSize:    10737418240,
		ChunkSize:     16777216,
		Encryption: EncryptionConfig{
			Mode:              "client_side",
			Algorithm:         "xchacha20-poly1305",
			KeyID:             "customer-managed",
			ManifestEncrypted: true,
		},
		PlacementPolicy: PlacementPolicy{
			Residency:         []string{"SG"},
			AllowedBackends:   []string{"wasabi-ap-southeast-1", "local-cell-1"},
			MinFailureDomains: 2,
			HotCache:          true,
		},
		Pieces: []Piece{
			{
				PieceID: "p_001",
				Hash:    "blake3:cafebabe",
				Backend: "wasabi-ap-southeast-1",
				Locator: "s3://zk-prod-a/pieces/p_001",
				State:   "active",
			},
		},
		MigrationState: MigrationState{
			Generation:     4,
			PrimaryBackend: "local-cell-1",
			CloudCopy:      "drain_after_30d",
		},
	}
}

func TestObjectManifest_JSONRoundTrip(t *testing.T) {
	orig := sampleManifest()

	data, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var got ObjectManifest
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

func TestObjectManifest_Validate(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		if err := sampleManifest().Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
	})

	cases := []struct {
		name   string
		mutate func(*ObjectManifest)
	}{
		{"missing tenant", func(m *ObjectManifest) { m.TenantID = "" }},
		{"missing bucket", func(m *ObjectManifest) { m.Bucket = "" }},
		{"missing key hash", func(m *ObjectManifest) { m.ObjectKeyHash = "" }},
		{"negative size", func(m *ObjectManifest) { m.ObjectSize = -1 }},
		{"zero chunk size", func(m *ObjectManifest) { m.ChunkSize = 0 }},
		{"piece missing id", func(m *ObjectManifest) { m.Pieces[0].PieceID = "" }},
		{"piece missing backend", func(m *ObjectManifest) { m.Pieces[0].Backend = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := sampleManifest()
			tc.mutate(m)
			if err := m.Validate(); err == nil {
				t.Fatalf("Validate: want error, got nil")
			}
		})
	}
}

func TestObjectManifest_KnownJSONShape(t *testing.T) {
	data, err := json.Marshal(sampleManifest())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var into map[string]any
	if err := json.Unmarshal(data, &into); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{
		"tenant_id", "bucket", "object_key_hash", "version_id",
		"object_size", "chunk_size", "encryption", "placement_policy",
		"pieces", "migration_state",
	} {
		if _, ok := into[key]; !ok {
			t.Fatalf("serialized manifest missing %q; got keys %v", key, keys(into))
		}
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
