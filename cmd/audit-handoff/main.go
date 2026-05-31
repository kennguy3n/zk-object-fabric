package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// main is intentionally a thin wrapper around run() so that
// defer-close + os.Exit composition is well-defined for tests.
// run() returns an exit code; main passes it through os.Exit.
// All errors are reported via fmt.Fprintln(os.Stderr, ...);
// there is no panic-recover layer.
func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("audit-handoff", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		repoRoot     string
		manifestPath string
		commitSHA    string
		skipMake     bool
		allowMissing bool
	)
	fs.StringVar(&repoRoot, "repo-root", "", "absolute path to the zk-object-fabric repo root (defaults to git rev-parse output)")
	fs.StringVar(&manifestPath, "manifest", "", "path to deploy/audit-handoff/manifest.yaml (defaults to <repo-root>/deploy/audit-handoff/manifest.yaml)")
	fs.StringVar(&commitSHA, "commit", "", "commit SHA to anchor the bundle to (defaults to git HEAD)")
	fs.BoolVar(&skipMake, "skip-make", false, "do not invoke any component's make_target; copy paths only")
	fs.BoolVar(&allowMissing, "allow-missing-optional", false, "omit MISSING placeholders for absent optional paths (default: write placeholders so the bundle records expected gaps)")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: audit-handoff [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Produces the external-auditor hand-off tarball described by\n")
		fmt.Fprintf(os.Stderr, "deploy/audit-handoff/manifest.yaml. Writes the tarball under\n")
		fmt.Fprintf(os.Stderr, "<repo-root>/<manifest.output_dir>.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already printed the error message;
		// just return a non-zero exit code.
		return 2
	}

	if repoRoot == "" {
		root, err := gitRepoRoot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-handoff: --repo-root not given and `git rev-parse --show-toplevel` failed: %v\n", err)
			return 1
		}
		repoRoot = root
	}
	if !filepath.IsAbs(repoRoot) {
		abs, err := filepath.Abs(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-handoff: resolve repo-root %q: %v\n", repoRoot, err)
			return 1
		}
		repoRoot = abs
	}
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		fmt.Fprintf(os.Stderr, "audit-handoff: %q does not look like the zk-object-fabric repo root (no go.mod): %v\n", repoRoot, err)
		return 1
	}

	if manifestPath == "" {
		manifestPath = filepath.Join(repoRoot, "deploy", "audit-handoff", "manifest.yaml")
	}
	m, err := LoadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-handoff: %v\n", err)
		return 1
	}

	if commitSHA == "" {
		sha, err := gitHeadSHA(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "audit-handoff: --commit not given and `git rev-parse HEAD` failed: %v\n", err)
			return 1
		}
		commitSHA = sha
	}

	res, err := Build(m, BundleOptions{
		RepoRoot:             repoRoot,
		ManifestPath:         manifestPath,
		CommitSHA:            commitSHA,
		SkipMake:             skipMake,
		AllowMissingOptional: allowMissing,
		Out:                  os.Stderr,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-handoff: build failed: %v\n", err)
		return 1
	}

	fmt.Fprintf(os.Stdout, "%s\n", res.OutputPath)
	fmt.Fprintf(os.Stderr, "audit-handoff: %d files, %d bytes uncompressed; included=%v missing=%v\n",
		res.FileCount, res.BytesUncompressed, res.ComponentsIncluded, res.ComponentsMissing)
	return 0
}

func gitRepoRoot() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitHeadSHA(repoRoot string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
