// Command conformance-aggregate parses third-party S3 conformance
// harness outputs (Ceph s3-tests XUnit XML and MinIO mint per-SDK
// JSON logs) and writes a single normalised matrix JSON suitable
// for dropping into the WS2.4 audit dossier.
//
// Inputs:
//
//	-s3tests-xunit  path to an s3-tests --with-xunit XML file
//	-mint-logs-dir  path to a mint-logs/{date}/ directory (parses
//	                every log.json beneath it)
//	-gateway-endpoint  optional URL stamped into the matrix
//	-gateway-sha       optional commit SHA stamped into the matrix
//
// Output:
//
//	-out  path to write the matrix JSON to (default stdout)
//
// Exit codes:
//
//	0  all entries are pass or unsupported (audit-acceptable)
//	1  one or more entries are failed or errored
//	64 invalid CLI usage
//	65 input file could not be parsed
//
// The exit-code policy mirrors the in-process matrix's AllPassed
// gate (tests/s3_conformance/runner.go) so the same CI/dossier
// scripts can consume either source.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/kennguy3n/zk-object-fabric/tests/conformance/external"
)

func main() {
	var (
		xunitPath       = flag.String("s3tests-xunit", "", "path to ceph s3-tests --with-xunit XML file (optional; at least one of -s3tests-xunit / -mint-logs-dir is required)")
		mintDir         = flag.String("mint-logs-dir", "", "path to MinIO mint logs directory (mint-logs/{date}/)")
		gatewayEndpoint = flag.String("gateway-endpoint", "", "URL of the gateway the harnesses ran against; stamped into the matrix for dossier attribution")
		gatewaySHA      = flag.String("gateway-sha", "", "commit SHA of the gateway under test; stamped into the matrix")
		outPath         = flag.String("out", "", "output matrix JSON path (default stdout)")
	)
	flag.Parse()

	if *xunitPath == "" && *mintDir == "" {
		fmt.Fprintln(os.Stderr, "conformance-aggregate: at least one of -s3tests-xunit / -mint-logs-dir is required")
		flag.Usage()
		os.Exit(64)
	}

	var parts [][]external.MatrixEntry

	if *xunitPath != "" {
		f, err := os.Open(*xunitPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: open xunit: %v\n", err)
			os.Exit(65)
		}
		entries, err := external.ParseS3TestsXUnit(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: parse xunit %s: %v\n", *xunitPath, err)
			os.Exit(65)
		}
		parts = append(parts, entries)
	}

	if *mintDir != "" {
		entries, err := external.ParseMintLogDir(*mintDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: parse mint dir %s: %v\n", *mintDir, err)
			os.Exit(65)
		}
		parts = append(parts, entries)
	}

	m := external.Aggregate(*gatewayEndpoint, *gatewaySHA, time.Now(), parts...)

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: open output %s: %v\n", *outPath, err)
			os.Exit(65)
		}
		defer f.Close()
		out = f
	}

	if err := m.WriteJSON(out); err != nil {
		fmt.Fprintf(os.Stderr, "conformance-aggregate: write matrix: %v\n", err)
		os.Exit(65)
	}

	c := m.Counts()
	fmt.Fprintf(os.Stderr, "conformance-aggregate: total=%d passed=%d unsupported=%d failed=%d errored=%d\n",
		c.Total, c.Passed, c.Unsupported, c.Failed, c.Errored)

	if !m.AllPassed() {
		os.Exit(1)
	}
}
