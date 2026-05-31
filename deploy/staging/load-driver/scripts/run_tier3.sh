#!/usr/bin/env bash
# Tier 3 (Linode + Wasabi) staging end-to-end harness runner.
#
# Invoked on the load-driver VM by the human operator. Runs the
# canonical staging invocation of benchmark-runner, writes the
# report JSON to /var/lib/zkof-loaddrv/reports, and re-applies the
# Tier 3 acceptance criteria via tier3-verify. Exits non-zero on
# verifier failure so the surrounding collect_evidence.sh sees the
# failed state and records it in the dossier.
#
# Required environment variables:
#   GATEWAY_ENDPOINT       https URL of the gateway NodeBalancer
#   STAGING_BUCKET         Wasabi bucket name (zkof-<region>-staging)
#   WASABI_KEY             per-region IAM access key
#   WASABI_SECRET          per-region IAM secret key
#   GATEWAY_SHA            commit SHA of the gateway binary under test
#
# Optional environment variables:
#   WASABI_ENDPOINT        defaults to https://s3.us-east-1.wasabisys.com
#   WASABI_REGION          defaults to us-east-1
#   TIER3_DURATION         defaults to 1h
#   TIER3_RPS              defaults to 12000
#   TIER3_CONCURRENCY      defaults to 128
#   TIER3_SEED_OBJECTS     defaults to 10000
#   REPORTS_DIR            defaults to /var/lib/zkof-loaddrv/reports
#   ENV                    defaults to tier3-staging
#
# The invocation parameters match docs/runbooks/load-testing.md §4
# verbatim: changing TIER3_RPS / TIER3_DURATION / TIER3_CONCURRENCY
# alters the published-SLA contract, so do not override them for
# the official staging run. The variables exist for diagnostic
# re-runs during incident triage only.

set -euo pipefail

require() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "run_tier3: required env var '$name' is unset" >&2
    exit 64
  fi
}

require GATEWAY_ENDPOINT
require STAGING_BUCKET
require WASABI_KEY
require WASABI_SECRET
require GATEWAY_SHA

WASABI_ENDPOINT="${WASABI_ENDPOINT:-https://s3.us-east-1.wasabisys.com}"
WASABI_REGION="${WASABI_REGION:-us-east-1}"
TIER3_DURATION="${TIER3_DURATION:-1h}"
TIER3_RPS="${TIER3_RPS:-12000}"
TIER3_CONCURRENCY="${TIER3_CONCURRENCY:-128}"
TIER3_SEED_OBJECTS="${TIER3_SEED_OBJECTS:-10000}"
REPORTS_DIR="${REPORTS_DIR:-/var/lib/zkof-loaddrv/reports}"
ENV_LABEL="${ENV:-tier3-staging}"

install -d -m 0755 "$REPORTS_DIR"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
report_path="$REPORTS_DIR/linode-wasabi-${timestamp}.json"
verdict_path="$REPORTS_DIR/linode-wasabi-${timestamp}.verdict.json"
run_log="$REPORTS_DIR/linode-wasabi-${timestamp}.run.log"
env_path="$REPORTS_DIR/linode-wasabi-${timestamp}.env.json"

# Capture the invocation environment so the dossier records
# exactly what was run. The path is part of the dossier the
# collector picks up.
cat > "$env_path" <<EOF
{
  "timestamp": "${timestamp}",
  "gateway_endpoint": "${GATEWAY_ENDPOINT}",
  "gateway_sha": "${GATEWAY_SHA}",
  "staging_bucket": "${STAGING_BUCKET}",
  "wasabi_endpoint": "${WASABI_ENDPOINT}",
  "wasabi_region": "${WASABI_REGION}",
  "duration": "${TIER3_DURATION}",
  "rps": ${TIER3_RPS},
  "concurrency": ${TIER3_CONCURRENCY},
  "seed_objects": ${TIER3_SEED_OBJECTS},
  "env": "${ENV_LABEL}",
  "load_driver_host": "$(hostname -f 2>/dev/null || hostname)",
  "load_driver_uname": "$(uname -srm)",
  "benchmark_runner_path": "$(command -v benchmark-runner || echo /opt/zkof-loaddrv/bin/benchmark-runner)",
  "benchmark_runner_sha256": "$(sha256sum "$(command -v benchmark-runner || echo /opt/zkof-loaddrv/bin/benchmark-runner)" | awk '{print $1}')",
  "tier3_verify_path": "$(command -v tier3-verify || echo /opt/zkof-loaddrv/bin/tier3-verify)",
  "tier3_verify_sha256": "$(sha256sum "$(command -v tier3-verify || echo /opt/zkof-loaddrv/bin/tier3-verify)" | awk '{print $1}')"
}
EOF

echo "==> Tier 3 staging run starting at $timestamp"
echo "    report  : $report_path"
echo "    verdict : $verdict_path"
echo "    log     : $run_log"

set +e
"$(command -v benchmark-runner || echo /opt/zkof-loaddrv/bin/benchmark-runner)" \
  -provider=wasabi \
  -wasabi-endpoint="$WASABI_ENDPOINT" \
  -wasabi-bucket="$STAGING_BUCKET" \
  -wasabi-region="$WASABI_REGION" \
  -wasabi-access-key="$WASABI_KEY" \
  -wasabi-secret-key="$WASABI_SECRET" \
  -duration="$TIER3_DURATION" \
  -rps="$TIER3_RPS" \
  -concurrency="$TIER3_CONCURRENCY" \
  -seed-objects="$TIER3_SEED_OBJECTS" \
  -out="$report_path" 2>&1 | tee "$run_log"
runner_status=${PIPESTATUS[0]}

if [ "$runner_status" -ne 0 ]; then
  echo "run_tier3: benchmark-runner exited $runner_status; report at $report_path (may be partial)" >&2
  # Still attempt verification — a partial report is informative for triage.
fi

echo "==> Running tier3-verify"
"$(command -v tier3-verify || echo /opt/zkof-loaddrv/bin/tier3-verify)" \
  -report "$report_path" \
  -out "$verdict_path" \
  -build-sha "$GATEWAY_SHA" \
  -env "$ENV_LABEL"
verify_status=$?
set -e

echo "==> Done."
echo "    benchmark-runner exit: $runner_status"
echo "    tier3-verify exit    : $verify_status"

# Surface the worst status as the script's exit code.
if [ "$runner_status" -ne 0 ]; then
  exit "$runner_status"
fi
exit "$verify_status"
