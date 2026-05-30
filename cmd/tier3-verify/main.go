// tier3-verify is the audit-side CLI gate for the Tier 3 (Linode +
// Wasabi) staging load run. It reads a benchmark.Report JSON
// produced by the benchmark-runner CLI and independently re-checks
// the five acceptance criteria the load-testing runbook gates the
// production promotion on. See tests/tier3verify for the full
// criteria list.
//
// The CLI prints a human-readable verdict to stderr and writes the
// structured Verdict JSON to stdout (or -out if supplied) so the
// dossier can capture both an operator-friendly summary and a
// machine-parseable record. Exit code is non-zero on any
// verification failure so CI pipelines and shell scripts can gate
// on it without parsing the JSON.
//
//	tier3-verify -report path/to/report.json \
//	             -build-sha "$(git rev-parse HEAD)" \
//	             -env tier3-staging \
//	             -out dossier/verdict.json
//
// The verifier re-applies the thresholds against the measured
// values in the report rather than trusting the report's own
// pass/fail flags, so a hand-edited JSON that flips AllPassed=true
// without changing the underlying numbers still fails the gate.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/kennguy3n/zk-object-fabric/tests/tier3verify"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "tier3-verify:", err)
		os.Exit(2)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tier3-verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	reportPath := fs.String("report", "", "path to the benchmark-runner Report JSON (required)")
	outPath := fs.String("out", "", "path to write the verdict JSON; '-' or empty writes to stdout")
	buildSHA := fs.String("build-sha", "", "build SHA of the gateway binary that produced the report")
	env := fs.String("env", "", "environment label (e.g. tier3-staging, prod, beta)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reportPath == "" {
		fs.Usage()
		return fmt.Errorf("-report is required")
	}

	f, err := os.Open(*reportPath)
	if err != nil {
		return fmt.Errorf("open report: %w", err)
	}
	defer f.Close()
	rpt, err := tier3verify.LoadReport(f)
	if err != nil {
		return err
	}

	verdict := tier3verify.Verify(rpt, tier3verify.Options{
		ReportPath:  *reportPath,
		BuildSHA:    *buildSHA,
		Environment: *env,
	})

	// Open the verdict output stream before printing the human
	// summary so a write failure surfaces before the operator sees
	// a green summary on stderr.
	var w io.Writer = stdout
	if *outPath != "" && *outPath != "-" {
		out, err := os.Create(*outPath)
		if err != nil {
			return fmt.Errorf("create verdict output: %w", err)
		}
		defer out.Close()
		w = out
	}
	if err := tier3verify.WriteVerdict(w, verdict); err != nil {
		return err
	}

	printSummary(stderr, verdict)
	if !verdict.AllPassed {
		return fmt.Errorf("tier 3 verification FAILED — see verdict for details")
	}
	return nil
}

func printSummary(w io.Writer, v tier3verify.Verdict) {
	fmt.Fprintln(w, "Tier 3 (Linode + Wasabi) staging verification")
	fmt.Fprintln(w, "==============================================")
	if v.ReportPath != "" {
		fmt.Fprintln(w, "Report     :", v.ReportPath)
	}
	if v.BuildSHA != "" {
		fmt.Fprintln(w, "Build SHA  :", v.BuildSHA)
	}
	if v.Environment != "" {
		fmt.Fprintln(w, "Environment:", v.Environment)
	}
	if v.StartedAt != "" {
		fmt.Fprintln(w, "Started    :", v.StartedAt)
	}
	if v.FinishedAt != "" {
		fmt.Fprintln(w, "Finished   :", v.FinishedAt)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Per-scenario verdict:")
	for _, sc := range v.Scenarios {
		status := "PASS"
		if !sc.Pass {
			status = "FAIL"
		}
		fmt.Fprintf(w, "  %-32s %s", sc.Name, status)
		if !sc.Present {
			fmt.Fprintln(w, "  (scenario missing from report)")
			continue
		}
		fmt.Fprintln(w)
		for _, m := range sc.Metrics {
			mstatus := "ok"
			if !m.Pass {
				mstatus = "FAIL"
			}
			fmt.Fprintf(w, "      %-30s %-4s measured=%-12g threshold=%g (%s, %s)\n",
				m.Metric, mstatus, m.Measured, m.Threshold, m.Direction, m.Unit)
			if m.Reason != "" {
				fmt.Fprintf(w, "          → %s\n", m.Reason)
			}
		}
	}
	fmt.Fprintln(w)
	if v.AllPassed {
		fmt.Fprintln(w, "VERDICT: PASS — Tier 3 SLA gates green, gateway build promotable.")
	} else {
		fmt.Fprintln(w, "VERDICT: FAIL — do NOT promote this build.")
		for _, f := range v.Failures {
			fmt.Fprintln(w, "  -", f)
		}
	}
	if v.ReportClaim != v.AllPassed {
		fmt.Fprintf(w, "  (note: report self-reported all_passed=%v; verifier disagreed)\n", v.ReportClaim)
	}
}
