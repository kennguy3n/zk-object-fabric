// SPDX-License-Identifier: Apache-2.0
package main

import (
	"os"
	"testing"

	"github.com/kennguy3n/zk-object-fabric/internal/config"
)

func TestResolveGatewayNodeID_ExplicitOverrideWins(t *testing.T) {
	if got := resolveGatewayNodeID("gw-7"); got != "gw-7" {
		t.Fatalf("resolveGatewayNodeID(%q) = %q, want gw-7", "gw-7", got)
	}
}

func TestResolveGatewayNodeID_FallsBackToHostname(t *testing.T) {
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("os.Hostname unavailable in this environment")
	}
	if got := resolveGatewayNodeID(""); got != host {
		t.Fatalf("resolveGatewayNodeID(\"\") = %q, want hostname %q", got, host)
	}
}

// TestResolveRebalancerNodeID_DelegatesToShared verifies the
// rebalancer resolver now shares the gateway-wide resolution: its
// explicit node_id still wins, and an empty one falls back to the
// same host-based identity the s3 handler and evaluator use.
func TestResolveRebalancerNodeID_DelegatesToShared(t *testing.T) {
	if got := resolveRebalancerNodeID(config.RebalancerConfig{NodeID: "rb-3"}); got != "rb-3" {
		t.Fatalf("rebalancer explicit node_id = %q, want rb-3", got)
	}
	if got := resolveRebalancerNodeID(config.RebalancerConfig{}); got != resolveGatewayNodeID("") {
		t.Fatalf("empty rebalancer node_id = %q, want shared gateway node id %q", got, resolveGatewayNodeID(""))
	}
}
