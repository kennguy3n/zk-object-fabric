package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// buildCLI compiles the CLI into a temp binary and returns its
// path. Using `go build` in the test avoids relying on a
// pre-installed binary while still exercising the real main()
// entry point and exit codes.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "conformance-aggregate")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func TestCLI_NoInputsFlag_Usage(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit, got success; output: %s", out)
	}
	if !strings.Contains(string(out), "at least one of") {
		t.Errorf("usage message missing expected substring; got: %s", out)
	}
	// Exit code 64 = invalid CLI usage.
	if cmd.ProcessState.ExitCode() != 64 {
		t.Errorf("exit code = %d, want 64", cmd.ProcessState.ExitCode())
	}
}

func TestCLI_AggregateBothInputs(t *testing.T) {
	bin := buildCLI(t)
	xunit := filepath.Join("..", "..", "tests", "conformance", "external", "testdata", "s3tests", "sample-xunit.xml")
	mintDir := filepath.Join("..", "..", "tests", "conformance", "external", "testdata", "mint")
	outPath := filepath.Join(t.TempDir(), "matrix.json")

	cmd := exec.Command(bin,
		"-s3tests-xunit", xunit,
		"-mint-logs-dir", mintDir,
		"-gateway-endpoint", "https://gw.test",
		"-gateway-sha", "deadbeef",
		"-out", outPath,
	)
	out, err := cmd.CombinedOutput()
	// The test fixtures include failed and errored entries, so
	// the CLI should exit 1.
	if err == nil {
		t.Fatalf("expected non-zero exit (fixtures have failures); output: %s", out)
	}
	if cmd.ProcessState.ExitCode() != 1 {
		t.Fatalf("exit code = %d, want 1; output: %s", cmd.ProcessState.ExitCode(), out)
	}
	if !strings.Contains(string(out), "passed=") || !strings.Contains(string(out), "failed=") {
		t.Errorf("stderr summary missing tally; got: %s", out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	var m struct {
		GatewayEndpoint string `json:"gateway_endpoint"`
		GatewaySHA      string `json:"gateway_sha"`
		Entries         []struct {
			Op     string `json:"op"`
			Source string `json:"source"`
			Status string `json:"status"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if m.GatewayEndpoint != "https://gw.test" || m.GatewaySHA != "deadbeef" {
		t.Errorf("metadata not stamped; got endpoint=%q sha=%q", m.GatewayEndpoint, m.GatewaySHA)
	}
	// 7 s3-tests entries + 10 mint entries = 17.
	if len(m.Entries) != 17 {
		t.Errorf("entry count = %d, want 17", len(m.Entries))
	}
	srcCount := make(map[string]int)
	for _, e := range m.Entries {
		srcCount[e.Source]++
	}
	if srcCount["ceph-s3-tests"] != 7 || srcCount["minio-mint"] != 10 {
		t.Errorf("source counts = %v, want ceph-s3-tests=7 minio-mint=10", srcCount)
	}
}

func TestCLI_OnlyMint_AllPassedExitsZero(t *testing.T) {
	// Build a mint-logs dir where every entry is PASS so the CLI
	// must exit 0.
	bin := buildCLI(t)
	root := t.TempDir()
	sdkDir := filepath.Join(root, "minio-go")
	if err := os.MkdirAll(sdkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(sdkDir, "log.json")
	logBody := `{"name":"minio-go","function":"PutObject","duration":5,"status":"PASS"}` + "\n" +
		`{"name":"minio-go","function":"GetObject","duration":4,"status":"PASS"}` + "\n" +
		`{"name":"minio-go","function":"Versioning","duration":1,"status":"NA"}` + "\n"
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatalf("write log.json: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), "matrix.json")

	cmd := exec.Command(bin, "-mint-logs-dir", root, "-out", outPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("CLI failed: %v; output: %s", err, out)
	}
	if cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", cmd.ProcessState.ExitCode(), out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read out: %v", err)
	}
	if !strings.Contains(string(body), `"status": "passed"`) {
		t.Errorf("matrix missing passed entries; got: %s", body)
	}
}
