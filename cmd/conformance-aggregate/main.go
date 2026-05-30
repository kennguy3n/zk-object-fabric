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
	// All work is delegated to run() so the only os.Exit call lives
	// at the bottom of main and every deferred resource (output
	// file Close + Sync, in particular) has a chance to fire on
	// every exit path. The pre-refactor shape used os.Exit(1)
	// directly after `defer f.Close()`, which skipped the close —
	// safe in practice for unbuffered *os.File writes but a footgun
	// the moment any caller wraps the output in a bufio.Writer.
	os.Exit(run())
}

// run is the testable entry point: it returns the desired exit
// code instead of calling os.Exit so all deferred resources run
// before the process terminates.
func run() int {
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
		return 64
	}

	var parts [][]external.MatrixEntry

	if *xunitPath != "" {
		f, err := os.Open(*xunitPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: open xunit: %v\n", err)
			return 65
		}
		entries, err := external.ParseS3TestsXUnit(f)
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: parse xunit %s: %v\n", *xunitPath, err)
			return 65
		}
		parts = append(parts, entries)
	}

	if *mintDir != "" {
		entries, err := external.ParseMintLogDir(*mintDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: parse mint dir %s: %v\n", *mintDir, err)
			return 65
		}
		parts = append(parts, entries)
	}

	m := external.Aggregate(*gatewayEndpoint, *gatewaySHA, time.Now(), parts...)

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: open output %s: %v\n", *outPath, err)
			return 65
		}
		// Defer Close so it fires on every return path below
		// (including the audit-fail return 1 at the end). Sync()
		// is also called explicitly after WriteJSON so the matrix
		// is on disk before the audit-pass tally is printed.
		defer f.Close()
		out = f
	}

	if err := m.WriteJSON(out); err != nil {
		fmt.Fprintf(os.Stderr, "conformance-aggregate: write matrix: %v\n", err)
		return 65
	}
	if out != os.Stdout {
		if err := out.Sync(); err != nil {
			fmt.Fprintf(os.Stderr, "conformance-aggregate: sync matrix: %v\n", err)
			return 65
		}
	}

	c := m.Counts()
	fmt.Fprintf(os.Stderr, "conformance-aggregate: total=%d passed=%d unsupported=%d failed=%d errored=%d\n",
		c.Total, c.Passed, c.Unsupported, c.Failed, c.Errored)

	if !m.AllPassed() {
		return 1
	}
	return 0
}
