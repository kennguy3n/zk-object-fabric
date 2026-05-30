#!/usr/bin/env bash
# Ceph s3-tests external conformance harness driver.
#
# Boots a venv, clones (or refreshes) the ceph/s3-tests repo at a
# pinned commit, writes an s3tests.conf pointing at the gateway,
# runs the canonical functional suite with our authoritative skip
# list, and archives the resulting XUnit XML report under
# $REPORTS_DIR/external/s3tests/{timestamp}.xml.
#
# The script is invoked on the operator workstation or a CI
# runner that has both Python 3.10+ and pip available. It does
# NOT need access to the Linode VM or Wasabi credentials — it
# only needs the gateway endpoint + S3 keys.
#
# Required environment variables:
#   GATEWAY_ENDPOINT       http(s) URL of the gateway
#   GATEWAY_ACCESS_KEY     S3 access key
#   GATEWAY_SECRET_KEY     S3 secret key
#
# Optional environment variables:
#   GATEWAY_BUCKET         informational only — Ceph s3-tests creates
#                          its own buckets dynamically per test, so this
#                          variable is not threaded into s3tests.conf.
#                          Kept symmetric with run_mint.sh's env block so
#                          operators set one consistent variable set.
#   GATEWAY_REGION         defaults to us-east-1
#   S3TESTS_REPO           defaults to https://github.com/ceph/s3-tests
#   S3TESTS_REV            defaults to master (pin a SHA in production)
#   S3TESTS_WORKDIR        defaults to /tmp/s3-tests
#   REPORTS_DIR            defaults to /var/lib/zkof-conf/reports
#   SKIP_TAGS_FILE         defaults to the bundled skip-tags.txt
#
# Exit codes mirror nosetests: 0 = all-pass-or-skip, 1 = one or
# more failures or errors. The XUnit XML is always written
# regardless of exit code so the aggregator can record failures.

set -euo pipefail

require() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    echo "run_s3tests: required env var '$name' is unset" >&2
    exit 64
  fi
}

require GATEWAY_ENDPOINT
require GATEWAY_ACCESS_KEY
require GATEWAY_SECRET_KEY
# GATEWAY_BUCKET is intentionally NOT required: Ceph s3-tests creates
# per-test buckets via its own bucket_prefix mechanism and ignores any
# operator-supplied name. We only log it for operator visibility.
GATEWAY_BUCKET="${GATEWAY_BUCKET:-<unset>}"

GATEWAY_REGION="${GATEWAY_REGION:-us-east-1}"
S3TESTS_REPO="${S3TESTS_REPO:-https://github.com/ceph/s3-tests}"
S3TESTS_REV="${S3TESTS_REV:-master}"
S3TESTS_WORKDIR="${S3TESTS_WORKDIR:-/tmp/s3-tests}"
REPORTS_DIR="${REPORTS_DIR:-/var/lib/zkof-conf/reports}"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SKIP_TAGS_FILE="${SKIP_TAGS_FILE:-${script_dir}/../s3tests/skip-tags.txt}"

if [ ! -f "$SKIP_TAGS_FILE" ]; then
  echo "run_s3tests: skip tags file not found at $SKIP_TAGS_FILE" >&2
  exit 65
fi

install -d -m 0755 "$REPORTS_DIR/external/s3tests"
timestamp=$(date -u +%Y%m%dT%H%M%SZ)
xunit_path="$REPORTS_DIR/external/s3tests/${timestamp}.xml"
run_log="$REPORTS_DIR/external/s3tests/${timestamp}.log"

# Clone or refresh the s3-tests repo at the pinned revision.
if [ ! -d "$S3TESTS_WORKDIR/.git" ]; then
  echo "==> Cloning ceph/s3-tests into $S3TESTS_WORKDIR"
  git clone "$S3TESTS_REPO" "$S3TESTS_WORKDIR"
fi
( cd "$S3TESTS_WORKDIR" && git fetch --quiet origin && git checkout --quiet "$S3TESTS_REV" )

# Bootstrap a venv inside the workdir so we don't pollute the
# operator's system python.
if [ ! -d "$S3TESTS_WORKDIR/venv" ]; then
  echo "==> Creating venv at $S3TESTS_WORKDIR/venv"
  python3 -m venv "$S3TESTS_WORKDIR/venv"
fi
# shellcheck disable=SC1091
source "$S3TESTS_WORKDIR/venv/bin/activate"
pip install --quiet --upgrade pip
pip install --quiet -r "$S3TESTS_WORKDIR/requirements.txt"
pip install --quiet -e "$S3TESTS_WORKDIR"

# Build s3tests.conf pointing at the gateway. The host field
# accepts a bare host or host:port; we extract from the
# GATEWAY_ENDPOINT URL. The default port is scheme-aware: 443
# for https://, 80 for http:// (mirrors run_mint.sh's logic).
case "$GATEWAY_ENDPOINT" in
  https://*) default_port=443; is_secure="yes" ;;
  http://*)  default_port=80;  is_secure="no"  ;;
  *)
    echo "run_s3tests: GATEWAY_ENDPOINT must start with http:// or https://" >&2
    exit 64
    ;;
esac
host=$(echo "$GATEWAY_ENDPOINT" | sed -e 's|^https\?://||' -e 's|/.*$||')
port=$(echo "$host" | awk -F: -v dp="$default_port" '{print ($2 == "") ? dp : $2}')
host_only=$(echo "$host" | awk -F: '{print $1}')

conf_path="$S3TESTS_WORKDIR/zkof-s3tests-${timestamp}.conf"
cat > "$conf_path" <<EOF
[DEFAULT]
host = ${host_only}
port = ${port}
is_secure = ${is_secure}

[s3 main]
display_name = zkof-conformance
user_id      = zkof-conformance
email        = devnull@example.com
api_name     = default
access_key   = ${GATEWAY_ACCESS_KEY}
secret_key   = ${GATEWAY_SECRET_KEY}
EOF

# Compose the nose `-a` attribute selector from the skip tags
# file. The file contains one tag per line (comments and blank
# lines stripped), and each becomes `!tag` in the selector.
selector=$(grep -vE '^\s*(#|$)' "$SKIP_TAGS_FILE" | sed 's/^/\!/' | paste -sd, -)
if [ -z "$selector" ]; then
  # No skip tags configured — run the entire functional suite.
  selector_arg=""
else
  selector_arg="-a $selector"
fi

echo "==> Running s3-tests at $timestamp"
echo "    gateway : $GATEWAY_ENDPOINT"
echo "    bucket  : $GATEWAY_BUCKET"
echo "    config  : $conf_path"
echo "    xunit   : $xunit_path"
echo "    skip    : ${selector:-<none>}"

# Run the suite. nosetests exits 0 on all-pass-or-skip, 1 on
# any failure or error — preserve that exit code as the script's
# exit code, but always archive the xunit first.
pushd "$S3TESTS_WORKDIR" > /dev/null
set +e
# shellcheck disable=SC2086
S3TEST_CONF="$conf_path" \
  "$S3TESTS_WORKDIR/venv/bin/nosetests" s3tests.functional \
    $selector_arg \
    --with-xunit \
    --xunit-file="$xunit_path" \
    2>&1 | tee "$run_log"
suite_status=${PIPESTATUS[0]}
set -e
popd > /dev/null

echo "==> s3-tests finished with exit $suite_status; xunit at $xunit_path"
exit "$suite_status"
