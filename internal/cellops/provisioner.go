// Package cellops scaffolds the operator-side cell provisioning
// workflow for B2B / sovereign tenants. The console exposes
// POST /api/tenants/{id}/dedicated-cells; the handler hands the
// request off to a CellProvisioner which mints a pending cell
// record, publishes any side-channel notifications (PagerDuty,
// runbook ticket), and eventually flips the record to "active"
// once the operator-side bring-up workflow completes.
//
// Phase 3 ships ManualProvisioner: it logs the request and stores
// a pending record in the DedicatedCellStore so the request is
// surfaced to operators without yet automating hardware
// allocation. Full automation (Terraform / Ansible) lives behind
// the same interface in Phase 4.
package cellops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// ProvisionStatus enumerates the lifecycle states a dedicated cell
// can be in. The set is intentionally narrow so the console
// frontend can render a stable status badge without per-deployment
// drift.
type ProvisionStatus string

const (
	StatusProvisioning   ProvisionStatus = "provisioning"
	StatusActive         ProvisionStatus = "active"
	StatusDecommissioning ProvisionStatus = "decommissioning"
)

// CellRequest is the input the console hands to the provisioner.
// All fields are operator-supplied via the POST body.
type CellRequest struct {
	// TenantID is the tenant the cell will be bound to. Required.
	TenantID string `json:"tenant_id"`

	// Region is the deploy region (e.g. "us-east-1", "eu-fra-1").
	// Required.
	Region string `json:"region"`

	// Country is the ISO-3166-1 alpha-2 country code the cell
	// resides in (e.g. "US", "DE"). Required for sovereign
	// data-residency contracts.
	Country string `json:"country"`

	// CapacityPetabytes is the raw provisioned capacity target
	// for the cell.
	CapacityPetabytes float64 `json:"capacity_petabytes"`

	// ErasureProfile is the placement_policy.ErasureProfile the
	// cell defaults to (e.g. "ec_6_2_local_dc"). Empty means
	// "use the platform default".
	ErasureProfile string `json:"erasure_profile"`

	// NodeCount is the planned storage-node count. Phase 3
	// requires 6 nodes minimum to satisfy a 6+2 erasure layout
	// without a hot reconstruction tax.
	NodeCount int `json:"node_count"`
}

// Validate reports whether the request is well-formed enough to
// hand to a provisioner. Implementations are free to apply
// stricter checks (e.g. region whitelists), but every Phase 3
// provisioner must enforce the floor below.
func (r CellRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.TenantID) == "":
		return errors.New("cellops: tenant_id is required")
	case strings.TrimSpace(r.Region) == "":
		return errors.New("cellops: region is required")
	case strings.TrimSpace(r.Country) == "":
		return errors.New("cellops: country is required")
	case r.NodeCount < 0:
		return errors.New("cellops: node_count must be non-negative")
	case r.CapacityPetabytes < 0:
		return errors.New("cellops: capacity_petabytes must be non-negative")
	}
	return nil
}

// CellStatus mirrors api/console.DedicatedCellDescriptor with
// extra operator-facing fields (NodeCount, ErasureProfile) the
// provisioner tracks. The console handler projects a CellStatus
// down to the descriptor shape the SPA renders.
type CellStatus struct {
	CellID            string          `json:"cell_id"`
	TenantID          string          `json:"tenant_id"`
	Region            string          `json:"region"`
	Country           string          `json:"country"`
	Status            ProvisionStatus `json:"status"`
	CapacityPetabytes float64         `json:"capacity_petabytes"`
	Utilization       float64         `json:"utilization"`
	ErasureProfile    string          `json:"erasure_profile"`
	NodeCount         int             `json:"node_count"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// CellProvisioner is the interface the console layer consumes. A
// production provisioner triggers Terraform / Ansible, whereas
// ManualProvisioner only logs the request and persists a pending
// record so operators can pick it up out-of-band.
type CellProvisioner interface {
	// ProvisionCell records a new cell-provisioning request and
	// returns the resulting CellStatus. Implementations must
	// validate the request via CellRequest.Validate.
	ProvisionCell(ctx context.Context, req CellRequest) (CellStatus, error)

	// DecommissionCell flips an existing cell to
	// "decommissioning" and triggers tear-down. Idempotent on
	// already-decommissioning cells.
	DecommissionCell(ctx context.Context, cellID string) error

	// CellStatus returns the current state of a cell.
	CellStatus(ctx context.Context, cellID string) (CellStatus, error)
}

// CellSink is the persistence hook ManualProvisioner writes
// pending cell records into. The console package's
// DedicatedCellStore satisfies a richer interface; cellops
// declares its own minimal contract here so the package does not
// import api/console (and create a dependency cycle).
type CellSink interface {
	UpsertDedicatedCell(ctx context.Context, status CellStatus) error
	GetDedicatedCell(ctx context.Context, cellID string) (CellStatus, bool, error)
	UpdateCellStatus(ctx context.Context, cellID string, status ProvisionStatus) error
}

// ManualProvisioner is the Phase 3 default. It records a pending
// cell row, logs the request so operators get a paged audit trail,
// and otherwise defers the actual hardware bring-up to the
// out-of-band runbook. Phase 4 drops a Terraform-backed
// provisioner in behind the same interface.
type ManualProvisioner struct {
	// Sink persists cell records. Required.
	Sink CellSink
	// Logger receives structured log lines for every operation.
	// Nil disables logging.
	Logger *log.Logger
	// Clock returns the current time. Defaults to time.Now.
	Clock func() time.Time
	// IDGenerator mints a new cell ID. Defaults to a 16-byte
	// crypto/rand hex string.
	IDGenerator func() (string, error)

	mu sync.Mutex
}

// NewManualProvisioner returns a ManualProvisioner bound to sink.
func NewManualProvisioner(sink CellSink) *ManualProvisioner {
	return &ManualProvisioner{
		Sink:        sink,
		Clock:       time.Now,
		IDGenerator: defaultCellID,
	}
}

func (p *ManualProvisioner) clock() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now()
}

func (p *ManualProvisioner) idGen() func() (string, error) {
	if p.IDGenerator != nil {
		return p.IDGenerator
	}
	return defaultCellID
}

func (p *ManualProvisioner) logf(format string, args ...interface{}) {
	if p.Logger == nil {
		return
	}
	p.Logger.Printf(format, args...)
}

// ProvisionCell implements CellProvisioner.
func (p *ManualProvisioner) ProvisionCell(ctx context.Context, req CellRequest) (CellStatus, error) {
	if p == nil || p.Sink == nil {
		return CellStatus{}, errors.New("cellops: ManualProvisioner.Sink is required")
	}
	if err := req.Validate(); err != nil {
		return CellStatus{}, err
	}
	id, err := p.idGen()()
	if err != nil {
		return CellStatus{}, fmt.Errorf("cellops: mint cell id: %w", err)
	}
	now := p.clock()
	status := CellStatus{
		CellID:            id,
		TenantID:          req.TenantID,
		Region:            req.Region,
		Country:           req.Country,
		Status:            StatusProvisioning,
		CapacityPetabytes: req.CapacityPetabytes,
		Utilization:       0,
		ErasureProfile:    req.ErasureProfile,
		NodeCount:         req.NodeCount,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.Sink.UpsertDedicatedCell(ctx, status); err != nil {
		return CellStatus{}, fmt.Errorf("cellops: persist cell: %w", err)
	}
	p.logf("cellops: provision request tenant=%s region=%s country=%s cell_id=%s capacity=%.2fPB nodes=%d profile=%s",
		req.TenantID, req.Region, req.Country, id, req.CapacityPetabytes, req.NodeCount, req.ErasureProfile)
	return status, nil
}

// DecommissionCell implements CellProvisioner.
func (p *ManualProvisioner) DecommissionCell(ctx context.Context, cellID string) error {
	if p == nil || p.Sink == nil {
		return errors.New("cellops: ManualProvisioner.Sink is required")
	}
	if cellID == "" {
		return errors.New("cellops: cell_id is required")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	current, ok, err := p.Sink.GetDedicatedCell(ctx, cellID)
	if err != nil {
		return fmt.Errorf("cellops: load cell: %w", err)
	}
	if !ok {
		return fmt.Errorf("cellops: cell %q not found", cellID)
	}
	if current.Status == StatusDecommissioning {
		return nil
	}
	if err := p.Sink.UpdateCellStatus(ctx, cellID, StatusDecommissioning); err != nil {
		return fmt.Errorf("cellops: update cell status: %w", err)
	}
	p.logf("cellops: decommission tenant=%s cell_id=%s", current.TenantID, cellID)
	return nil
}

// CellStatus implements CellProvisioner.
func (p *ManualProvisioner) CellStatus(ctx context.Context, cellID string) (CellStatus, error) {
	if p == nil || p.Sink == nil {
		return CellStatus{}, errors.New("cellops: ManualProvisioner.Sink is required")
	}
	if cellID == "" {
		return CellStatus{}, errors.New("cellops: cell_id is required")
	}
	current, ok, err := p.Sink.GetDedicatedCell(ctx, cellID)
	if err != nil {
		return CellStatus{}, fmt.Errorf("cellops: load cell: %w", err)
	}
	if !ok {
		return CellStatus{}, fmt.Errorf("cellops: cell %q not found", cellID)
	}
	return current, nil
}

func defaultCellID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cell-" + hex.EncodeToString(buf), nil
}
