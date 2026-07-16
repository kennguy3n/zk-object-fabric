#!/usr/bin/env bash
# MinIO mint external conformance harness driver.
#
# Pulls the minio/mint:edge container, runs it against the
# gateway, and archives the per-SDK log.json files under
# $REPORTS_DIR/external/mint/{timestamp}/{sdk}/log.json.
#
# Required environment variables:
#   GATEWAY_ENDPOINT       https URL of the gateway
#   GATEWAY_ACCESS_KEY     S3 access key
#   GATEWAY_SECRET_KEY     S3 secret key
#
# Optional environment variables:
#   GATEWAY_REGION         defaults to us-east-1
#   MINT_IMAGE             defaults to minio/mint:edge
#   MINT_MODE              defaults to core
#                          (other: full, quick — see runbook)
#   REPORTS_DIR            defaults to /var/lib/zkof-conf/reports
#
# Exit codes:
#   0  mint reported no defects across any SDK
#   1  mint reported at least one defect (the aggregator will
#      attribute it to the right SDK)
#   64 missing required env var
#   65 docker not available or image pull failed

set -euo pipefail

require() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "run_mint: required env var '$name' is unset" >&2
    exit 64
  fi
}

require GATEWAY_ENDPOINT
require GATEWAY_ACCESS_KEY
require GATEWAY_SECRET_KEY

GATEWAY_REGION="${GATEWAY_REGION:-us-east-1}"
MINT_IMAGE="${MINT_IMAGE:-minio/mint:edge}"
MINT_MODE="${MINT_MODE:-core}"
REPORTS_DIR="${REPORTS_DIR:-/var/lib/zkof-conf/reports}"

if ! command -v docker > /dev/null 2>&1; then
  echo "run_mint: docker is required but not in PATH" >&2
  exit 65
fi

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
mint_log_root="$REPORTS_DIR/external/mint/${timestamp}"
install -d -m 0755 "$mint_log_root"

# Extract host:port from the endpoint. mint expects SERVER_ENDPOINT
# in the form "host:port".
host=$(echo "$GATEWAY_ENDPOINT" | sed -e 's|^https\?://||' -e 's|/.*$||')
case "$host" in
  *:*) server_endpoint="$host" ;;
  *)
    if [[ "$GATEWAY_ENDPOINT" == https://* ]]; then
      server_endpoint="${host}:443"
    else
      server_endpoint="${host}:80"
    fi
    ;;
esac
enable_https="0"
if [[ "$GATEWAY_ENDPOINT" == https://* ]]; then
  enable_https="1"
fi

echo "==> Running mint at $timestamp"
echo "    image    : $MINT_IMAGE"
echo "    mode     : $MINT_MODE"
echo "    endpoint : $server_endpoint (https=$enable_https)"
echo "    region   : $GATEWAY_REGION"
echo "    logs     : $mint_log_root"

# Pull explicitly so the script fails fast if the image is not
# available (instead of failing inside `docker run` after the
# config is already on disk).
docker pull "$MINT_IMAGE" > /dev/null

# Run mint. The container writes per-SDK logs to /mint/log inside
# the container; we bind-mount the host log root over it.
set +e
docker run --rm \
  -e "SERVER_ENDPOINT=$server_endpoint" \
  -e "ACCESS_KEY=$GATEWAY_ACCESS_KEY" \
  -e "SECRET_KEY=$GATEWAY_SECRET_KEY" \
  -e "ENABLE_HTTPS=$enable_https" \
  -e "SERVER_REGION=$GATEWAY_REGION" \
  -e "MINT_MODE=$MINT_MODE" \
  -v "$mint_log_root:/mint/log" \
  "$MINT_IMAGE"
mint_status=$?
set -e

echo "==> mint finished with exit $mint_status; per-SDK logs at $mint_log_root"
exit "$mint_status"
