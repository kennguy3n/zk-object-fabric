package erasure_coding

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func mustEncoder(t *testing.T, p ErasureCodingProfile) *Encoder {
	t.Helper()
	enc, err := NewEncoder(p)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	return enc
}

func TestEncoder_RoundTrip_SmallProfile(t *testing.T) {
	enc := mustEncoder(t, ErasureCodingProfile{
		Name: "6+2", DataShards: 6, ParityShards: 2,
		StripeSize: 64 * 1024,
	})
	sizes := []int{0, 1, 31, 64, 8 * 1024, 64*1024 - 1, 64 * 1024, 64*1024 + 1, 10 * 64 * 1024}
	for _, n := range sizes {
		body := make([]byte, n)
		_, _ = rand.Read(body)
		shards, err := enc.Encode(body)
		if err != nil {
			t.Fatalf("Encode(%d): %v", n, err)
		}
		expected := ((n + objectHeaderSize) + enc.StripeLength() - 1) / enc.StripeLength() * enc.profile.TotalShards()
		if n == 0 {
			expected = enc.profile.TotalShards()
		}
		if len(shards) != expected {
			t.Fatalf("size=%d: shards=%d, want %d", n, len(shards), expected)
		}
		got, err := enc.Decode(shards)
		if err != nil {
			t.Fatalf("Decode(%d): %v", n, err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("size=%d: round-trip mismatch (got %d bytes)", n, len(got))
		}
	}
}

func TestEncoder_ReconstructsFromMinShards(t *testing.T) {
	enc := mustEncoder(t, ErasureCodingProfile{
		Name: "6+2", DataShards: 6, ParityShards: 2,
		StripeSize: 32 * 1024,
	})
	body := make([]byte, 200*1024)
	_, _ = rand.Read(body)
	shards, err := enc.Encode(body)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// Drop the first two shards of each stripe — the maximum loss
	// the 6+2 profile tolerates per stripe.
	stripesInObj := map[int]bool{}
	for _, s := range shards {
		stripesInObj[s.StripeIndex] = true
	}
	for s := range stripesInObj {
		count := 0
		for i, shard := range shards {
			if shard.StripeIndex != s {
				continue
			}
			shards[i].Bytes = nil
			count++
			if count == 2 {
				break
			}
		}
	}

	got, err := enc.Decode(shards)
	if err != nil {
		t.Fatalf("Decode after max loss: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch after loss (got %d bytes)", len(got))
	}
}

func TestEncoder_FailsBelowMinShards(t *testing.T) {
	enc := mustEncoder(t, ErasureCodingProfile{
		Name: "6+2", DataShards: 6, ParityShards: 2,
		StripeSize: 32 * 1024,
	})
	body := make([]byte, 4*1024)
	_, _ = rand.Read(body)
	shards, err := enc.Encode(body)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Drop 3 shards from the only stripe (> parity tolerance).
	dropped := 0
	for i := range shards {
		if shards[i].StripeIndex == 0 && dropped < 3 {
			shards[i].Bytes = nil
			dropped++
		}
	}
	if _, err := enc.Decode(shards); err == nil {
		t.Fatal("expected Decode to fail below min shards")
	}
}

func TestEncoder_DecodeReader_RoundTrip(t *testing.T) {
	enc := mustEncoder(t, ErasureCodingProfile{
		Name: "6+2", DataShards: 6, ParityShards: 2,
		StripeSize: 16 * 1024,
	})
	body := make([]byte, 40*1024)
	_, _ = rand.Read(body)
	shards, err := enc.Encode(body)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	numStripes := 0
	for _, s := range shards {
		if s.StripeIndex+1 > numStripes {
			numStripes = s.StripeIndex + 1
		}
	}
	total := enc.Profile().TotalShards()
	readers := make([][]io.Reader, numStripes)
	for s := 0; s < numStripes; s++ {
		readers[s] = make([]io.Reader, total)
	}
	for _, sh := range shards {
		readers[sh.StripeIndex][sh.ShardIndex] = bytes.NewReader(sh.Bytes)
	}

	got, err := enc.DecodeReader(numStripes, readers)
	if err != nil {
		t.Fatalf("DecodeReader: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round-trip mismatch via reader path")
	}
}

func TestEncoder_AllStandardProfilesValidate(t *testing.T) {
	for _, p := range StandardProfiles() {
		if _, err := NewEncoder(p); err != nil {
			t.Errorf("profile %s: %v", p.Name, err)
		}
	}
}
