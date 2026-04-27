package repair

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CephHealthClient polls a Ceph manager REST API
// (`/api/v0.1/health` or `/api/health/full`) and surfaces
// HealthSignal values for the repair queue.
//
// The mapping from Ceph's health document to AffectedPieceIDs is
// deliberately delegated to the PieceResolver callback: most
// production deployments will translate degraded PG IDs to piece
// IDs via a CRUSH-aware index living outside this package.
type CephHealthClient struct {
	Endpoint     string
	HTTP         *http.Client
	AuthToken    string
	PieceResolver func(ctx context.Context, pgs []string, osds []int) ([]string, error)
}

// cephHealthDoc is the minimal schema we read out of the
// dashboard / mgr API. Only the fields the resolver consumes are
// declared; unknown JSON keys are ignored.
type cephHealthDoc struct {
	Status string `json:"status"` // HEALTH_OK / HEALTH_WARN / HEALTH_ERR
	Checks map[string]struct {
		Severity string `json:"severity"`
		Summary  struct {
			Message string `json:"message"`
		} `json:"summary"`
	} `json:"checks"`
	OsdMap struct {
		Down []int `json:"down"`
	} `json:"osdmap"`
	PgMap struct {
		Degraded []string `json:"degraded"`
	} `json:"pgmap"`
}

// Poll implements HealthSignalSource.
func (c *CephHealthClient) Poll(ctx context.Context) (HealthSignal, error) {
	if c == nil || c.Endpoint == "" {
		return HealthSignal{}, fmt.Errorf("repair: ceph endpoint not configured")
	}
	cli := c.HTTP
	if cli == nil {
		cli = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Endpoint, nil)
	if err != nil {
		return HealthSignal{}, err
	}
	if c.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.AuthToken)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		return HealthSignal{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return HealthSignal{}, fmt.Errorf("ceph health: http %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var doc cephHealthDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return HealthSignal{}, err
	}
	sig := HealthSignal{
		Healthy:    doc.Status == "" || doc.Status == "HEALTH_OK",
		ObservedAt: time.Now(),
	}
	if sig.Healthy {
		return sig, nil
	}
	if c.PieceResolver != nil {
		pieces, err := c.PieceResolver(ctx, doc.PgMap.Degraded, doc.OsdMap.Down)
		if err != nil {
			return HealthSignal{}, err
		}
		sig.AffectedPieceIDs = pieces
	}
	return sig, nil
}
