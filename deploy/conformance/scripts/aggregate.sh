#!/usr/bin/env bash
# Aggregate s3-tests + mint outputs into a unified matrix JSON.
#
# Invoked after run_s3tests.sh and run_mint.sh have produced their
# respective artifact directories under $REPORTS_DIR/external/.
# Picks up the latest xunit XML and the latest mint logs dir,
# runs cmd/conformance-aggregate, and writes
# $REPORTS_DIR/external/matrix-{timestamp}.json.
#
# Optional environment variables:
#   GATEWAY_ENDPOINT       stamped into the matrix (recommended)
#   GATEWAY_SHA            stamped into the matrix (recommended)
#   REPORTS_DIR            defaults to /var/lib/zkof-conf/reports
#   AGGREGATE_BIN          defaults to $(command -v conformance-aggregate)
#                          or /opt/zkof-conf/bin/conformance-aggregate
#
# Exit codes mirror conformance-aggregate: 0 = audit-pass (all
# entries pass or unsupported), 1 = at least one failed or
# errored entry.

set -euo pipefail

REPORTS_DIR="${REPORTS_DIR:-/var/lib/zkof-conf/reports}"
AGGREGATE_BIN="${AGGREGATE_BIN:-$(command -v conformance-aggregate || echo /opt/zkof-conf/bin/conformance-aggregate)}"

if [ ! -x "$AGGREGATE_BIN" ]; then
  echo "aggregate: conformance-aggregate binary not found at $AGGREGATE_BIN" >&2
  exit 65
fi

# Pick the most-recent XUnit (one file per s3-tests run) and the
# most-recent mint-logs subdir (one dir per mint run). Using
# find -printf with null separators handles unusual filenames
# (operator-supplied $REPORTS_DIR paths with spaces, etc.).
xunit=""
if [ -d "$REPORTS_DIR/external/s3tests" ]; then
  xunit=$(find "$REPORTS_DIR/external/s3tests" -maxdepth 1 -type f -name '*.xml' -printf '%T@ %p\0' 2>/dev/null \
          | sort -z -rn | head -z -n1 | cut -z -d' ' -f2- | tr -d '\0')
fi
mint_dir=""
if [ -d "$REPORTS_DIR/external/mint" ]; then
  mint_dir=$(find "$REPORTS_DIR/external/mint" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %p\0' 2>/dev/null \
             | sort -z -rn | head -z -n1 | cut -z -d' ' -f2- | tr -d '\0')
fi

if [ -z "$xunit" ] && [ -z "$mint_dir" ]; then
  echo "aggregate: no s3-tests or mint outputs found under $REPORTS_DIR/external" >&2
  exit 64
fi

timestamp=$(date -u +%Y%m%dT%H%M%SZ)
out_path="$REPORTS_DIR/external/matrix-${timestamp}.json"

args=()
if [ -n "$xunit" ]; then
  args+=(-s3tests-xunit "$xunit")
fi
if [ -n "$mint_dir" ]; then
  args+=(-mint-logs-dir "$mint_dir")
fi
if [ -n "${GATEWAY_ENDPOINT:-}" ]; then
  args+=(-gateway-endpoint "$GATEWAY_ENDPOINT")
fi
if [ -n "${GATEWAY_SHA:-}" ]; then
  args+=(-gateway-sha "$GATEWAY_SHA")
fi
args+=(-out "$out_path")

echo "==> Aggregating into $out_path"
echo "    s3tests xunit : ${xunit:-<none>}"
echo "    mint logs dir : ${mint_dir:-<none>}"

set +e
"$AGGREGATE_BIN" "${args[@]}"
agg_status=$?
set -e

echo "==> Aggregate finished with exit $agg_status; matrix at $out_path"
exit "$agg_status"
