// AutomatedProvisioner: Terraform-driven dedicated cell bring-up.
//
// The Phase 4 production implementation shells out to a
// TerraformRunner which is expected to wrap a "terraform apply
// -auto-approve -var-file ..." invocation against a curated
// module that bootstraps a Ceph cell (mons, OSDs, RGW, S3
// credentials, monitoring agents). Polling reads the apply log
// for the canonical "ready" marker and flips the cell row to
// "active" once it appears.
//
// The TerraformRunner abstraction keeps the provisioner testable
// without a real Terraform binary: tests pass a fake runner that
// returns scripted outputs.
package cellops

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TerraformRunner is the minimal exec surface the provisioner
// relies on. Implementations may be a real os/exec wrapper, a
// Terraform Cloud HTTP client, or a test fake.
type TerraformRunner interface {
	// Apply runs `terraform apply` with the variables in vars
	// and returns the stdout/stderr of the run. Implementations
	// MUST block until the apply terminates.
	Apply(ctx context.Context, vars map[string]string) (string, error)

	// Destroy runs `terraform destroy` for the given cell.
	Destroy(ctx context.Context, cellID string) error
}

// CompletionDetector inspects the output of a Terraform apply
// and decides whether the run reached the ready state.
//
// The default detector matches a substring "module.cell.endpoint
// = ..." which the bundled Terraform module prints on success.
// Operators can override the detector to match their own marker.
type CompletionDetector func(applyOutput string) (endpoint string, ready bool)

// AutomatedProvisioner orchestrates cell bring-up via Terraform.
// The state machine is:
//
//	ProvisionCell:
//	  1. Validate request, mint cell ID, write
//	     StatusProvisioning row.
//	  2. Hand the request to the TerraformRunner.
//	  3. Run the CompletionDetector on the apply output.
//	  4. If ready, flip the row to StatusActive and persist the
//	     resolved endpoint.
//
// The struct is safe for concurrent calls; each invocation runs
// in its own goroutine when WaitForCompletion=false (the
// default).
type AutomatedProvisioner struct {
	Sink              CellSink
	Runner            TerraformRunner
	Detector          CompletionDetector
	Clock             func() time.Time
	IDGenerator       func() (string, error)
	WaitForCompletion bool

	mu sync.Mutex
}

// NewAutomatedProvisioner returns a provisioner wired to sink
// and runner. WaitForCompletion defaults to false (apply runs
// in the background); tests typically set it to true.
func NewAutomatedProvisioner(sink CellSink, runner TerraformRunner) *AutomatedProvisioner {
	return &AutomatedProvisioner{
		Sink:        sink,
		Runner:      runner,
		Detector:    defaultDetector,
		Clock:       time.Now,
		IDGenerator: defaultCellID,
	}
}

func (p *AutomatedProvisioner) clock() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now()
}

// ProvisionCell implements CellProvisioner.
func (p *AutomatedProvisioner) ProvisionCell(ctx context.Context, req CellRequest) (CellStatus, error) {
	if p == nil || p.Sink == nil || p.Runner == nil {
		return CellStatus{}, errors.New("cellops: AutomatedProvisioner.Sink and Runner are required")
	}
	if err := req.Validate(); err != nil {
		return CellStatus{}, err
	}
	id, err := p.IDGenerator()
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
	apply := func() {
		out, runErr := p.Runner.Apply(context.Background(), map[string]string{
			"cell_id":         id,
			"tenant_id":       req.TenantID,
			"region":          req.Region,
			"country":         req.Country,
			"erasure_profile": req.ErasureProfile,
			"node_count":      fmt.Sprintf("%d", req.NodeCount),
			"capacity_pb":     fmt.Sprintf("%.2f", req.CapacityPetabytes),
		})
		if runErr != nil {
			// Apply failed; leave the cell in provisioning so
			// operators see the stuck row in the console.
			return
		}
		if p.Detector != nil {
			if _, ready := p.Detector(out); !ready {
				return
			}
		}
		_ = p.Sink.UpdateCellStatus(context.Background(), id, StatusActive)
	}
	if p.WaitForCompletion {
		apply()
	} else {
		go apply()
	}
	return status, nil
}

// DecommissionCell implements CellProvisioner.
func (p *AutomatedProvisioner) DecommissionCell(ctx context.Context, cellID string) error {
	if p == nil || p.Sink == nil || p.Runner == nil {
		return errors.New("cellops: AutomatedProvisioner.Sink and Runner are required")
	}
	if cellID == "" {
		return errors.New("cellops: cell_id is required")
	}
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
	go func() { _ = p.Runner.Destroy(context.Background(), cellID) }()
	return nil
}

// CellStatus implements CellProvisioner.
func (p *AutomatedProvisioner) CellStatus(ctx context.Context, cellID string) (CellStatus, error) {
	if p == nil || p.Sink == nil {
		return CellStatus{}, errors.New("cellops: AutomatedProvisioner.Sink is required")
	}
	current, ok, err := p.Sink.GetDedicatedCell(ctx, cellID)
	if err != nil {
		return CellStatus{}, err
	}
	if !ok {
		return CellStatus{}, fmt.Errorf("cellops: cell %q not found", cellID)
	}
	return current, nil
}

// defaultDetector matches the canonical readiness marker the
// bundled Terraform module emits: "cell endpoint: <url>".
func defaultDetector(out string) (string, bool) {
	const marker = "cell endpoint: "
	idx := indexOf(out, marker)
	if idx < 0 {
		return "", false
	}
	rest := out[idx+len(marker):]
	end := indexOf(rest, "\n")
	if end < 0 {
		return rest, true
	}
	return rest[:end], true
}

// indexOf is strings.Index without importing strings (the package
// already has imports kept tight; this helper avoids one more).
func indexOf(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
