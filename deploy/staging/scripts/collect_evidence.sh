#!/usr/bin/env bash
# Assembles the Tier 3 staging audit-dossier directory from the
# artifacts produced by the staging run. Runs on the operator
# workstation (not on any Linode VM) and SSH/scp's into the
# gateway nodes and load driver to fetch logs.
#
# Required environment variables:
#   GATEWAY_NODES        space-separated list of gateway SSH hosts
#                        (e.g. "gw1.example gw2.example gw3.example")
#   LOAD_DRIVER_HOST     SSH host of the load driver
#   STAGING_BUCKET       Wasabi bucket name (zkof-<region>-staging)
#
# Optional environment variables:
#   SSH_USER             defaults to ubuntu
#   WASABI_ENDPOINT      defaults to https://s3.us-east-1.wasabisys.com
#   WASABI_REGION        defaults to us-east-1
#   WASABI_LOG_BUCKET    defaults to ${STAGING_BUCKET}-logs
#   WASABI_KEY           Wasabi access key (only required for log download)
#   WASABI_SECRET        Wasabi secret key (only required for log download)
#   DOSSIER_ROOT         defaults to deploy/staging/evidence
#   RUN_WINDOW_START     ISO8601 UTC of run start (defaults to "1 hour ago")
#   RUN_WINDOW_END       ISO8601 UTC of run end (defaults to "now")
#
# The dossier directory is laid out per
# deploy/staging/evidence/README.md and a top-level
# tier3-evidence-<timestamp>-<sha>.tar.gz is produced for shipping.

set -euo pipefail

require() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "collect_evidence: required env var '$name' is unset" >&2
    exit 64
  fi
}

require GATEWAY_NODES
require LOAD_DRIVER_HOST
require STAGING_BUCKET

SSH_USER="${SSH_USER:-ubuntu}"
WASABI_ENDPOINT="${WASABI_ENDPOINT:-https://s3.us-east-1.wasabisys.com}"
WASABI_REGION="${WASABI_REGION:-us-east-1}"
WASABI_LOG_BUCKET="${WASABI_LOG_BUCKET:-${STAGING_BUCKET}-logs}"
DOSSIER_ROOT="${DOSSIER_ROOT:-deploy/staging/evidence}"
RUN_WINDOW_START="${RUN_WINDOW_START:-$(date -u -d "1 hour ago" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -v-1H +%Y-%m-%dT%H:%M:%SZ)}"
RUN_WINDOW_END="${RUN_WINDOW_END:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"

timestamp=$(date -u +%Y%m%dT%H%M%SZ)

# Pull the most recent verdict from the load driver to discover
# the gateway SHA the run was tagged with. The dossier directory
# name embeds that SHA so the artifact is self-identifying.
echo "==> Discovering gateway SHA from load driver $LOAD_DRIVER_HOST"
remote_verdict=$(ssh "$SSH_USER@$LOAD_DRIVER_HOST" \
  'ls -t /var/lib/zkof-loaddrv/reports/*.verdict.json 2>/dev/null | head -n 1')
if [ -z "$remote_verdict" ]; then
  echo "collect_evidence: no *.verdict.json found on load driver; run run_tier3.sh first" >&2
  exit 65
fi
# shellcheck disable=SC2029  # remote_verdict is intentionally expanded
# locally; the remote shell receives the literal path.
gateway_sha=$(ssh "$SSH_USER@$LOAD_DRIVER_HOST" \
  "jq -r '.build_sha // \"unknown\"' '$remote_verdict'")
verdict_basename=$(basename "$remote_verdict" .verdict.json)

dossier_dir="${DOSSIER_ROOT}/${timestamp}-${gateway_sha:0:12}"
install -d -m 0755 "$dossier_dir"
echo "==> Dossier directory: $dossier_dir"
echo "    gateway sha       : $gateway_sha"
echo "    matched run       : $verdict_basename"

# --- 01: load-driver report + verdict + env JSON ---------------------
install -d -m 0755 "$dossier_dir/01-load-driver"
scp "$SSH_USER@$LOAD_DRIVER_HOST:/var/lib/zkof-loaddrv/reports/${verdict_basename}.json" \
    "$dossier_dir/01-load-driver/report.json"
scp "$SSH_USER@$LOAD_DRIVER_HOST:/var/lib/zkof-loaddrv/reports/${verdict_basename}.verdict.json" \
    "$dossier_dir/01-load-driver/verdict.json"
scp "$SSH_USER@$LOAD_DRIVER_HOST:/var/lib/zkof-loaddrv/reports/${verdict_basename}.env.json" \
    "$dossier_dir/01-load-driver/env.json"
scp "$SSH_USER@$LOAD_DRIVER_HOST:/var/lib/zkof-loaddrv/reports/${verdict_basename}.run.log" \
    "$dossier_dir/01-load-driver/run.log"

# --- 02: gateway journal logs ---------------------------------------
install -d -m 0755 "$dossier_dir/02-gateway-journals"
node_idx=0
for node in $GATEWAY_NODES; do
  node_idx=$((node_idx + 1))
  printf -v node_label "node-%02d-%s" "$node_idx" "$node"
  echo "==> Capturing journalctl from $node"
  # shellcheck disable=SC2029  # run window is intentionally expanded
  # locally; the remote shell receives the resolved timestamps.
  ssh "$SSH_USER@$node" \
    "sudo journalctl -u zk-gateway --since '$RUN_WINDOW_START' --until '$RUN_WINDOW_END' --no-pager" \
    > "$dossier_dir/02-gateway-journals/${node_label}.log"
  ssh "$SSH_USER@$node" "curl -fsSL http://127.0.0.1:8080/internal/health" \
    > "$dossier_dir/02-gateway-journals/${node_label}.health.json" || true
  ssh "$SSH_USER@$node" "curl -fsSL http://127.0.0.1:8080/metrics" \
    > "$dossier_dir/02-gateway-journals/${node_label}.metrics.prom" || true
done

# --- 03: Wasabi access logs (best-effort) ----------------------------
install -d -m 0755 "$dossier_dir/03-wasabi-access-logs"
if [ -n "${WASABI_KEY:-}" ] && [ -n "${WASABI_SECRET:-}" ]; then
  echo "==> Syncing Wasabi access logs from s3://$WASABI_LOG_BUCKET"
  AWS_ACCESS_KEY_ID="$WASABI_KEY" \
  AWS_SECRET_ACCESS_KEY="$WASABI_SECRET" \
  AWS_DEFAULT_REGION="$WASABI_REGION" \
    aws --endpoint-url "$WASABI_ENDPOINT" s3 sync \
        "s3://$WASABI_LOG_BUCKET/" \
        "$dossier_dir/03-wasabi-access-logs/"
else
  echo "==> WASABI_KEY/SECRET unset; skipping wasabi log sync (operator must attach manually)"
  cat > "$dossier_dir/03-wasabi-access-logs/README.md" <<'EOF'
Wasabi access logs were NOT auto-synced because WASABI_KEY and/or
WASABI_SECRET were not exported when collect_evidence.sh ran.

The operator MUST manually `aws s3 sync` the access logs from
the staging logs bucket into this directory and re-run
`make tier3-evidence-rehash` before shipping the dossier.
EOF
fi

# --- 04: NodeBalancer config / health snapshot ----------------------
install -d -m 0755 "$dossier_dir/04-nodebalancer"
cat > "$dossier_dir/04-nodebalancer/README.md" <<EOF
This directory holds the NodeBalancer health-check history covering
the run window ($RUN_WINDOW_START to $RUN_WINDOW_END). Populate
manually via the Linode API:

    linode-cli nodebalancers list --json > nodebalancers.json
    linode-cli nodebalancers configs-list <id> --json > configs.json
    linode-cli nodebalancers stats <id> --json > stats.json
EOF

# --- 05: environment summary ----------------------------------------
cat > "$dossier_dir/00-environment.json" <<EOF
{
  "dossier_timestamp": "${timestamp}",
  "gateway_sha": "${gateway_sha}",
  "gateway_nodes": "$(echo "$GATEWAY_NODES" | tr ' ' ',')",
  "load_driver_host": "${LOAD_DRIVER_HOST}",
  "staging_bucket": "${STAGING_BUCKET}",
  "wasabi_endpoint": "${WASABI_ENDPOINT}",
  "wasabi_region": "${WASABI_REGION}",
  "wasabi_log_bucket": "${WASABI_LOG_BUCKET}",
  "run_window_start": "${RUN_WINDOW_START}",
  "run_window_end": "${RUN_WINDOW_END}",
  "collected_by": "$(id -un)@$(hostname -f 2>/dev/null || hostname)"
}
EOF

# --- 06: MANIFEST + tarball -----------------------------------------
echo "==> Generating MANIFEST.txt"
# Write the manifest to a temp file OUTSIDE the dossier directory
# first, then move it in. This avoids the find-scan-vs-write race
# (sha256sum would otherwise hash a half-written MANIFEST.txt that
# was still being appended to in the same pipeline) and also lets
# us roll back cleanly on a mid-run failure.
manifest_tmp=$(mktemp)
trap 'rm -f "$manifest_tmp"' EXIT
( cd "$dossier_dir" && \
  find . -type f -print0 | sort -z | \
    xargs -0 sha256sum ) > "$manifest_tmp"
mv "$manifest_tmp" "$dossier_dir/MANIFEST.txt"
trap - EXIT

tarball="${dossier_dir}.tar.gz"
echo "==> Producing tarball $tarball"
tar -C "$(dirname "$dossier_dir")" -czf "$tarball" "$(basename "$dossier_dir")"

# Hash the tarball itself so the audit dossier can be verified by
# downstream consumers without re-extracting.
sha256sum "$tarball" > "${tarball}.sha256"
echo "==> SHA256: $(cat "${tarball}.sha256")"
echo
echo "Dossier complete: $tarball"
