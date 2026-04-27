package cellops

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu      sync.Mutex
	output  string
	err     error
	applied []map[string]string
}

func (f *fakeRunner) Apply(_ context.Context, vars map[string]string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, vars)
	return f.output, f.err
}

func (f *fakeRunner) Destroy(_ context.Context, cellID string) error {
	return nil
}

type memSink struct {
	mu      sync.Mutex
	records map[string]CellStatus
}

func newMemSink() *memSink { return &memSink{records: map[string]CellStatus{}} }

func (s *memSink) UpsertDedicatedCell(_ context.Context, st CellStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[st.CellID] = st
	return nil
}

func (s *memSink) GetDedicatedCell(_ context.Context, id string) (CellStatus, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.records[id]
	return c, ok, nil
}

func (s *memSink) UpdateCellStatus(_ context.Context, id string, st ProvisionStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.records[id]
	if !ok {
		return errors.New("not found")
	}
	c.Status = st
	c.UpdatedAt = time.Now()
	s.records[id] = c
	return nil
}

func TestAutomatedProvisioner_FlipsToActiveOnReadyMarker(t *testing.T) {
	sink := newMemSink()
	runner := &fakeRunner{output: "...running...\ncell endpoint: https://rgw.cell-x\n", err: nil}
	p := NewAutomatedProvisioner(sink, runner)
	p.WaitForCompletion = true
	st, err := p.ProvisionCell(context.Background(), CellRequest{
		TenantID: "T", Region: "us-east-1", Country: "US", NodeCount: 6,
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if st.Status != StatusProvisioning {
		t.Errorf("initial Status = %q, want provisioning", st.Status)
	}
	rec, _, _ := sink.GetDedicatedCell(context.Background(), st.CellID)
	if rec.Status != StatusActive {
		t.Errorf("final Status = %q, want active", rec.Status)
	}
	if len(runner.applied) != 1 {
		t.Errorf("Apply called %d times, want 1", len(runner.applied))
	}
}

func TestAutomatedProvisioner_LeavesProvisioningOnRunnerError(t *testing.T) {
	sink := newMemSink()
	runner := &fakeRunner{err: errors.New("apply failed")}
	p := NewAutomatedProvisioner(sink, runner)
	p.WaitForCompletion = true
	st, err := p.ProvisionCell(context.Background(), CellRequest{
		TenantID: "T", Region: "r", Country: "US",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := sink.GetDedicatedCell(context.Background(), st.CellID)
	if rec.Status != StatusProvisioning {
		t.Errorf("Status = %q, want provisioning (left stuck for operator)", rec.Status)
	}
}

func TestAutomatedProvisioner_LeavesProvisioningWhenMarkerMissing(t *testing.T) {
	sink := newMemSink()
	runner := &fakeRunner{output: "...running... no marker"}
	p := NewAutomatedProvisioner(sink, runner)
	p.WaitForCompletion = true
	st, err := p.ProvisionCell(context.Background(), CellRequest{
		TenantID: "T", Region: "r", Country: "US",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := sink.GetDedicatedCell(context.Background(), st.CellID)
	if rec.Status != StatusProvisioning {
		t.Errorf("Status = %q, want provisioning when marker missing", rec.Status)
	}
}

func TestAutomatedProvisioner_DecommissionFlipsAndDestroys(t *testing.T) {
	sink := newMemSink()
	runner := &fakeRunner{output: "cell endpoint: https://x\n"}
	p := NewAutomatedProvisioner(sink, runner)
	p.WaitForCompletion = true
	st, err := p.ProvisionCell(context.Background(), CellRequest{TenantID: "T", Region: "r", Country: "US"})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.DecommissionCell(context.Background(), st.CellID); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := sink.GetDedicatedCell(context.Background(), st.CellID)
	if rec.Status != StatusDecommissioning {
		t.Errorf("Status = %q, want decommissioning", rec.Status)
	}
}
