// benchmark-runner is the CLI front-end for the zk-object-fabric
// load-test harness. It builds a StorageProvider from flags, runs
// the requested benchmark suite (default: tests/benchmark.DefaultSuite),
// emits a JSON report, and exits non-zero on target failure.
//
// Supported providers:
//
//   - local_fs_dev       (no credentials; used by CI smoke runs)
//   - ceph_rgw           (-rgw-endpoint, -rgw-bucket, -rgw-region,
//                        -rgw-access-key, -rgw-secret-key)
//   - wasabi             (-wasabi-endpoint, -wasabi-bucket,
//                        -wasabi-region, -wasabi-access-key,
//                        -wasabi-secret-key)
//   - backblaze_b2       (analogous -b2-* flags)
//   - cloudflare_r2      (analogous -r2-* flags)
//   - aws_s3             (-s3-region, -s3-bucket; credentials picked
//                        up from the default AWS chain)
//
// Examples:
//
//	benchmark-runner -provider=local_fs_dev -root=/tmp/bench \
//	    -duration=10s -rps=200 -out=report.json
//
//	benchmark-runner -provider=ceph_rgw \
//	    -rgw-endpoint=http://localhost:8888 -rgw-bucket=bench \
//	    -rgw-access-key=$RGW_KEY -rgw-secret-key=$RGW_SECRET \
//	    -duration=60s -rps=1000 -out=report.json
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kennguy3n/zk-object-fabric/providers"
	"github.com/kennguy3n/zk-object-fabric/providers/aws_s3"
	"github.com/kennguy3n/zk-object-fabric/providers/backblaze_b2"
	"github.com/kennguy3n/zk-object-fabric/providers/ceph_rgw"
	"github.com/kennguy3n/zk-object-fabric/providers/cloudflare_r2"
	"github.com/kennguy3n/zk-object-fabric/providers/local_fs_dev"
	"github.com/kennguy3n/zk-object-fabric/providers/wasabi"
	"github.com/kennguy3n/zk-object-fabric/tests/benchmark"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("benchmark-runner: %v", err)
	}
}

func run() error {
	flags := parseFlags()

	provider, err := buildProvider(flags)
	if err != nil {
		return fmt.Errorf("build provider %q: %w", flags.provider, err)
	}

	suite, err := selectSuite(flags)
	if err != nil {
		return err
	}

	runner := &benchmark.SustainedRunner{
		Provider:           provider,
		Concurrency:        flags.concurrency,
		SeedObjects:        flags.seedObjects,
		DurationOverride:   flags.durationOverride,
		RPSOverride:        flags.rpsOverride,
		MaxObjectSizeBytes: flags.maxObjectBytes,
		FailureLimit:       flags.failureLimit,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, err := runSuite(ctx, suite, runner)
	if err != nil {
		return fmt.Errorf("run suite %q: %w", suite.Name, err)
	}

	if err := writeReport(flags.outPath, report, flags.indentJSON); err != nil {
		return fmt.Errorf("write report: %w", err)
	}

	logReport(report)

	if !report.AllPassed {
		return fmt.Errorf("one or more scenarios failed their declared targets; see %s", flags.outPath)
	}
	return nil
}

type flagset struct {
	provider         string
	scenarioFilter   string
	outPath          string
	indentJSON       bool
	concurrency      int
	seedObjects      int
	durationOverride time.Duration
	rpsOverride      int
	maxObjectBytes   int64
	failureLimit     int

	// local_fs_dev
	root string

	// ceph_rgw
	rgwEndpoint  string
	rgwBucket    string
	rgwRegion    string
	rgwAccessKey string
	rgwSecretKey string

	// wasabi
	wasabiEndpoint  string
	wasabiBucket    string
	wasabiRegion    string
	wasabiAccessKey string
	wasabiSecretKey string

	// backblaze_b2
	b2Endpoint  string
	b2Bucket    string
	b2Region    string
	b2AccessKey string
	b2SecretKey string

	// cloudflare_r2
	r2Endpoint  string
	r2AccountID string
	r2Bucket    string
	r2AccessKey string
	r2SecretKey string

	// aws_s3
	s3Region string
	s3Bucket string
}

func parseFlags() *flagset {
	f := &flagset{}
	flag.StringVar(&f.provider, "provider", "local_fs_dev", "provider to drive: local_fs_dev|ceph_rgw|wasabi|backblaze_b2|cloudflare_r2|aws_s3")
	flag.StringVar(&f.scenarioFilter, "scenario", "", "optional substring filter: only scenarios whose Name contains this string will run")
	flag.StringVar(&f.outPath, "out", "report.json", "path to write the JSON report to")
	flag.BoolVar(&f.indentJSON, "indent", true, "indent the JSON output")
	flag.IntVar(&f.concurrency, "concurrency", 0, "worker goroutines (0 = auto from rps)")
	flag.IntVar(&f.seedObjects, "seed-objects", 0, "seeded working-set size (0 = auto)")
	flag.DurationVar(&f.durationOverride, "duration", 0, "per-scenario duration override (0 = use scenario default)")
	flag.IntVar(&f.rpsOverride, "rps", 0, "per-scenario target RPS override (0 = use scenario default)")
	flag.Int64Var(&f.maxObjectBytes, "max-object-bytes", 0, "cap on per-scenario object size in bytes (0 = unlimited)")
	flag.IntVar(&f.failureLimit, "failure-limit", 0, "consecutive errors before aborting the run (0 = 64)")

	flag.StringVar(&f.root, "root", "", "local_fs_dev: filesystem root for piece storage (defaults to a temp dir)")

	flag.StringVar(&f.rgwEndpoint, "rgw-endpoint", os.Getenv("RGW_ENDPOINT"), "ceph_rgw: full RGW base URL")
	flag.StringVar(&f.rgwBucket, "rgw-bucket", os.Getenv("RGW_BUCKET"), "ceph_rgw: bucket name")
	flag.StringVar(&f.rgwRegion, "rgw-region", os.Getenv("RGW_REGION"), "ceph_rgw: Ceph zonegroup label")
	flag.StringVar(&f.rgwAccessKey, "rgw-access-key", os.Getenv("RGW_ACCESS_KEY"), "ceph_rgw: access key")
	flag.StringVar(&f.rgwSecretKey, "rgw-secret-key", os.Getenv("RGW_SECRET_KEY"), "ceph_rgw: secret key")

	flag.StringVar(&f.wasabiEndpoint, "wasabi-endpoint", os.Getenv("WASABI_ENDPOINT"), "wasabi: full endpoint URL")
	flag.StringVar(&f.wasabiBucket, "wasabi-bucket", os.Getenv("WASABI_BUCKET"), "wasabi: bucket name")
	flag.StringVar(&f.wasabiRegion, "wasabi-region", os.Getenv("WASABI_REGION"), "wasabi: AWS-style region")
	flag.StringVar(&f.wasabiAccessKey, "wasabi-access-key", os.Getenv("WASABI_ACCESS_KEY"), "wasabi: access key")
	flag.StringVar(&f.wasabiSecretKey, "wasabi-secret-key", os.Getenv("WASABI_SECRET_KEY"), "wasabi: secret key")

	flag.StringVar(&f.b2Endpoint, "b2-endpoint", os.Getenv("B2_ENDPOINT"), "backblaze_b2: full endpoint URL")
	flag.StringVar(&f.b2Bucket, "b2-bucket", os.Getenv("B2_BUCKET"), "backblaze_b2: bucket name")
	flag.StringVar(&f.b2Region, "b2-region", os.Getenv("B2_REGION"), "backblaze_b2: region")
	flag.StringVar(&f.b2AccessKey, "b2-access-key", os.Getenv("B2_ACCESS_KEY"), "backblaze_b2: access key")
	flag.StringVar(&f.b2SecretKey, "b2-secret-key", os.Getenv("B2_SECRET_KEY"), "backblaze_b2: secret key")

	flag.StringVar(&f.r2Endpoint, "r2-endpoint", os.Getenv("R2_ENDPOINT"), "cloudflare_r2: full endpoint URL")
	flag.StringVar(&f.r2AccountID, "r2-account-id", os.Getenv("R2_ACCOUNT_ID"), "cloudflare_r2: Cloudflare account ID")
	flag.StringVar(&f.r2Bucket, "r2-bucket", os.Getenv("R2_BUCKET"), "cloudflare_r2: bucket name")
	flag.StringVar(&f.r2AccessKey, "r2-access-key", os.Getenv("R2_ACCESS_KEY"), "cloudflare_r2: access key")
	flag.StringVar(&f.r2SecretKey, "r2-secret-key", os.Getenv("R2_SECRET_KEY"), "cloudflare_r2: secret key")

	flag.StringVar(&f.s3Region, "s3-region", os.Getenv("S3_REGION"), "aws_s3: region")
	flag.StringVar(&f.s3Bucket, "s3-bucket", os.Getenv("S3_BUCKET"), "aws_s3: bucket name")

	flag.Parse()
	return f
}

func buildProvider(f *flagset) (providers.StorageProvider, error) {
	switch strings.ToLower(f.provider) {
	case "local_fs_dev", "local", "fs":
		root := f.root
		if root == "" {
			d, err := os.MkdirTemp("", "zkof-bench-*")
			if err != nil {
				return nil, fmt.Errorf("create temp root: %w", err)
			}
			root = d
		}
		log.Printf("benchmark-runner: local_fs_dev root=%s", root)
		return local_fs_dev.New(root)

	case "ceph_rgw", "rgw":
		if f.rgwEndpoint == "" || f.rgwBucket == "" || f.rgwAccessKey == "" || f.rgwSecretKey == "" {
			return nil, errors.New("ceph_rgw requires -rgw-endpoint, -rgw-bucket, -rgw-access-key, -rgw-secret-key (or RGW_* env)")
		}
		return ceph_rgw.New(ceph_rgw.Config{
			Endpoint:  f.rgwEndpoint,
			Region:    f.rgwRegion,
			Bucket:    f.rgwBucket,
			AccessKey: f.rgwAccessKey,
			SecretKey: f.rgwSecretKey,
		})

	case "wasabi":
		if f.wasabiEndpoint == "" || f.wasabiBucket == "" || f.wasabiAccessKey == "" || f.wasabiSecretKey == "" {
			return nil, errors.New("wasabi requires -wasabi-endpoint, -wasabi-bucket, -wasabi-access-key, -wasabi-secret-key (or WASABI_* env)")
		}
		return wasabi.New(wasabi.Config{
			Endpoint:  f.wasabiEndpoint,
			Region:    f.wasabiRegion,
			Bucket:    f.wasabiBucket,
			AccessKey: f.wasabiAccessKey,
			SecretKey: f.wasabiSecretKey,
		})

	case "backblaze_b2", "b2":
		if f.b2Endpoint == "" || f.b2Bucket == "" || f.b2AccessKey == "" || f.b2SecretKey == "" {
			return nil, errors.New("backblaze_b2 requires -b2-endpoint, -b2-bucket, -b2-access-key, -b2-secret-key (or B2_* env)")
		}
		return backblaze_b2.New(backblaze_b2.Config{
			Endpoint:  f.b2Endpoint,
			Region:    f.b2Region,
			Bucket:    f.b2Bucket,
			AccessKey: f.b2AccessKey,
			SecretKey: f.b2SecretKey,
		})

	case "cloudflare_r2", "r2":
		if f.r2Bucket == "" || f.r2AccessKey == "" || f.r2SecretKey == "" {
			return nil, errors.New("cloudflare_r2 requires -r2-bucket, -r2-access-key, -r2-secret-key (or R2_* env)")
		}
		return cloudflare_r2.New(cloudflare_r2.Config{
			Endpoint:  f.r2Endpoint,
			AccountID: f.r2AccountID,
			Bucket:    f.r2Bucket,
			AccessKey: f.r2AccessKey,
			SecretKey: f.r2SecretKey,
		})

	case "aws_s3", "s3":
		if f.s3Region == "" || f.s3Bucket == "" {
			return nil, errors.New("aws_s3 requires -s3-region and -s3-bucket")
		}
		return aws_s3.New(aws_s3.Config{
			Region: f.s3Region,
			Bucket: f.s3Bucket,
		})

	default:
		return nil, fmt.Errorf("unknown provider %q", f.provider)
	}
}

func selectSuite(f *flagset) (benchmark.Suite, error) {
	suite := benchmark.DefaultSuite()
	if f.scenarioFilter == "" {
		return suite, nil
	}
	var kept []benchmark.Scenario
	for _, sc := range suite.Scenarios {
		if strings.Contains(sc.Name, f.scenarioFilter) {
			kept = append(kept, sc)
		}
	}
	if len(kept) == 0 {
		return benchmark.Suite{}, fmt.Errorf("scenario filter %q matched 0 scenarios", f.scenarioFilter)
	}
	suite.Scenarios = kept
	return suite, nil
}

func runSuite(ctx context.Context, suite benchmark.Suite, runner *benchmark.SustainedRunner) (*benchmark.Report, error) {
	type result struct {
		rep *benchmark.Report
		err error
	}
	done := make(chan result, 1)
	go func() {
		rep, err := benchmark.RunSuite(suite, runner)
		done <- result{rep, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-done:
		return r.rep, r.err
	}
}

func writeReport(path string, report *benchmark.Report, indent bool) error {
	if path == "" {
		path = "report.json"
	}
	var (
		data []byte
		err  error
	)
	if indent {
		data, err = json.MarshalIndent(report, "", "  ")
	} else {
		data, err = json.Marshal(report)
	}
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}

func logReport(report *benchmark.Report) {
	log.Printf("benchmark-runner: suite=%s scenarios=%d all_passed=%v",
		report.Suite, len(report.Scenarios), report.AllPassed)
	for _, sc := range report.Scenarios {
		status := "PASS"
		if !sc.Pass {
			status = "FAIL"
		}
		log.Printf("  [%s] scenario=%s metrics=%d failures=%d",
			status, sc.Name, len(sc.Results), len(sc.Failures))
		for _, fmsg := range sc.Failures {
			log.Printf("    - %s", fmsg)
		}
		for _, r := range sc.Results {
			log.Printf("    metric=%s value=%.6f unit=%s duration=%s",
				r.Metric, r.Value, r.Labels["unit"], r.Duration)
		}
	}
}
