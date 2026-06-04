package sse

import "testing"

func TestConfigEmpty(t *testing.T) {
	if !(Config{}).Empty() {
		t.Error("zero Config.Empty() = false, want true")
	}
	if (Config{Algorithm: AES256}).Empty() {
		t.Error("AES256 Config.Empty() = true, want false")
	}
	if (Config{Algorithm: AWSKMS, KMSMasterKeyID: "arn:key"}).Empty() {
		t.Error("aws:kms Config.Empty() = true, want false")
	}
}

func TestConfigValid(t *testing.T) {
	cases := []struct {
		name    string
		cfg     Config
		wantErr bool
	}{
		{"aes256", Config{Algorithm: AES256}, false},
		{"aes256 with kms key id rejected", Config{Algorithm: AES256, KMSMasterKeyID: "arn:key"}, true},
		{"aws:kms without key id", Config{Algorithm: AWSKMS}, false},
		{"aws:kms with key id", Config{Algorithm: AWSKMS, KMSMasterKeyID: "arn:aws:kms:us-east-1:111122223333:key/abc"}, false},
		{"aws:kms with bucket key", Config{Algorithm: AWSKMS, BucketKeyEnabled: true}, false},
		{"empty algorithm rejected", Config{}, true},
		{"unknown algorithm rejected", Config{Algorithm: Algorithm("rot13")}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Valid()
			if tc.wantErr && err == nil {
				t.Errorf("Valid() = nil, want error for %+v", tc.cfg)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Valid() = %v, want nil for %+v", err, tc.cfg)
			}
		})
	}
}
