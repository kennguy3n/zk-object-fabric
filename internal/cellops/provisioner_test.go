package cellops

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeSink struct {
	cells map[string]CellStatus
}

func newFakeSink() *fakeSink { return &fakeSink{cells: map[string]CellStatus{}} }

func (s *fakeSink) UpsertDedicatedCell(ctx context.Context, c CellStatus) error {
	s.cells[c.CellID] = c
	return nil
}

func (s *fakeSink) GetDedicatedCell(ctx context.Context, id string) (CellStatus, bool, error) {
	c, ok := s.cells[id]
	return c, ok, nil
}

func (s *fakeSink) UpdateCellStatus(ctx context.Context, id string, status ProvisionStatus) error {
	c, ok := s.cells[id]
	if !ok {
		return errors.New("not found")
	}
	c.Status = status
	c.UpdatedAt = time.Now()
	s.cells[id] = c
	return nil
}

func TestManualProvisioner_ProvisionPersists(t *testing.T) {
	sink := newFakeSink()
	p := NewManualProvisioner(sink)
	p.IDGenerator = func() (string, error) { return "cell-fixed-1", nil }
	p.Clock = func() time.Time { return time.Date(2026, 4, 25, 0, 0, 0, 0, time.UTC) }

	st, err := p.ProvisionCell(context.Background(), CellRequest{
		TenantID:          "t1",
		Region:            "us-east-1",
		Country:           "US",
		CapacityPetabytes: 1.5,
		ErasureProfile:    "ec_6_2_local_dc",
		NodeCount:         8,
	})
	if err != nil {
		t.Fatalf("ProvisionCell: %v", err)
	}
	if st.CellID != "cell-fixed-1" || st.Status != StatusProvisioning {
		t.Fatalf("unexpected status: %+v", st)
	}
	got, ok, err := sink.GetDedicatedCell(context.Background(), "cell-fixed-1")
	if err != nil || !ok {
		t.Fatalf("GetDedicatedCell: ok=%v err=%v", ok, err)
	}
	if got.TenantID != "t1" || got.Region != "us-east-1" || got.Country != "US" {
		t.Fatalf("unexpected persisted cell: %+v", got)
	}
	if got.NodeCount != 8 || got.ErasureProfile != "ec_6_2_local_dc" {
		t.Fatalf("operator fields not persisted: %+v", got)
	}
}

func TestManualProvisioner_ValidatesRequest(t *testing.T) {
	p := NewManualProvisioner(newFakeSink())
	cases := map[string]CellRequest{
		"missing tenant":  {Region: "r", Country: "C"},
		"missing region":  {TenantID: "t", Country: "C"},
		"missing country": {TenantID: "t", Region: "r"},
		"negative nodes":  {TenantID: "t", Region: "r", Country: "C", NodeCount: -1},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := p.ProvisionCell(context.Background(), req); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}

func TestManualProvisioner_DecommissionFlipsStatus(t *testing.T) {
	sink := newFakeSink()
	p := NewManualProvisioner(sink)
	p.IDGenerator = func() (string, error) { return "cell-1", nil }

	if _, err := p.ProvisionCell(context.Background(), CellRequest{
		TenantID: "t", Region: "r", Country: "C",
	}); err != nil {
		t.Fatalf("ProvisionCell: %v", err)
	}
	if err := p.DecommissionCell(context.Background(), "cell-1"); err != nil {
		t.Fatalf("DecommissionCell: %v", err)
	}
	st, err := p.CellStatus(context.Background(), "cell-1")
	if err != nil {
		t.Fatalf("CellStatus: %v", err)
	}
	if st.Status != StatusDecommissioning {
		t.Fatalf("status = %q want decommissioning", st.Status)
	}
	// Idempotent.
	if err := p.DecommissionCell(context.Background(), "cell-1"); err != nil {
		t.Fatalf("idempotent DecommissionCell: %v", err)
	}
}

func TestManualProvisioner_DecommissionUnknown(t *testing.T) {
	p := NewManualProvisioner(newFakeSink())
	if err := p.DecommissionCell(context.Background(), "missing"); err == nil {
		t.Fatalf("expected error for missing cell")
	}
}

func TestManualProvisioner_NilSink(t *testing.T) {
	p := &ManualProvisioner{}
	if _, err := p.ProvisionCell(context.Background(), CellRequest{TenantID: "t", Region: "r", Country: "C"}); err == nil {
		t.Fatalf("expected error for nil sink")
	}
}

func TestDefaultCellIDDistinct(t *testing.T) {
	a, err := defaultCellID()
	if err != nil {
		t.Fatalf("defaultCellID: %v", err)
	}
	b, err := defaultCellID()
	if err != nil {
		t.Fatalf("defaultCellID: %v", err)
	}
	if a == b {
		t.Fatalf("expected distinct IDs, got %q twice", a)
	}
}
