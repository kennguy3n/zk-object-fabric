package cellops

import (
	"context"
	"errors"
	"testing"
)

type stubLister struct {
	all []CellStatus
	err error
}

func (s *stubLister) ListAllCells(context.Context) ([]CellStatus, error) {
	return s.all, s.err
}

func (s *stubLister) ListCellsByTenant(_ context.Context, tenantID string) ([]CellStatus, error) {
	out := []CellStatus{}
	for _, c := range s.all {
		if c.TenantID == tenantID {
			out = append(out, c)
		}
	}
	return out, s.err
}

func (s *stubLister) GetDedicatedCell(_ context.Context, cellID string) (CellStatus, bool, error) {
	for _, c := range s.all {
		if c.CellID == cellID {
			return c, true, nil
		}
	}
	return CellStatus{}, false, nil
}

func TestCellRegistry_ListActiveCellsFiltersByStatus(t *testing.T) {
	s := &stubLister{all: []CellStatus{
		{CellID: "a", Status: StatusActive},
		{CellID: "b", Status: StatusProvisioning},
		{CellID: "c", Status: StatusActive},
		{CellID: "d", Status: StatusDecommissioning},
	}}
	got, err := NewCellRegistry(s).ListActiveCells(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].CellID != "a" || got[1].CellID != "c" {
		t.Errorf("got %+v", got)
	}
}

func TestCellRegistry_GetCellsByTenant(t *testing.T) {
	s := &stubLister{all: []CellStatus{
		{CellID: "a", TenantID: "T1", Status: StatusActive},
		{CellID: "b", TenantID: "T2", Status: StatusActive},
		{CellID: "c", TenantID: "T1", Status: StatusProvisioning},
	}}
	got, err := NewCellRegistry(s).GetCellsByTenant(context.Background(), "T1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d, want 2", len(got))
	}
}

func TestCellRegistry_GetCellByID(t *testing.T) {
	s := &stubLister{all: []CellStatus{{CellID: "x", Status: StatusActive}}}
	r := NewCellRegistry(s)
	c, ok, err := r.GetCellByID(context.Background(), "x")
	if err != nil || !ok || c.CellID != "x" {
		t.Errorf("got (%+v, %v, %v)", c, ok, err)
	}
	_, ok, _ = r.GetCellByID(context.Background(), "missing")
	if ok {
		t.Errorf("missing cell should return ok=false")
	}
}

func TestCellRegistry_NilSafe(t *testing.T) {
	r := NewCellRegistry(nil)
	if _, err := r.ListActiveCells(context.Background()); err == nil {
		t.Errorf("expected error on nil store")
	}
}

func TestCellRegistry_PropagatesStoreError(t *testing.T) {
	s := &stubLister{err: errors.New("db down")}
	if _, err := NewCellRegistry(s).ListActiveCells(context.Background()); err == nil {
		t.Errorf("expected error to propagate")
	}
}
