#!/usr/bin/env bash
# Top-level B2B dedicated-cell provisioner. Drives the end-to-end
# flow from hardware allocation to console registration.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

CONSOLE_URL=""
ADMIN_TOKEN=""
SPEC=""
DECOMMISSION_CELL=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --console-url)   CONSOLE_URL="$2"; shift 2;;
    --admin-token)   ADMIN_TOKEN="$2"; shift 2;;
    --spec)          SPEC="$2"; shift 2;;
    --decommission)  DECOMMISSION_CELL="$2"; shift 2;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

if [ -n "$DECOMMISSION_CELL" ]; then
  echo "==> decommissioning cell $DECOMMISSION_CELL"
  curl -fsS -X DELETE \
    -H "Authorization: Bearer $ADMIN_TOKEN" \
    "$CONSOLE_URL/api/admin/dedicated-cells/$DECOMMISSION_CELL"
  echo "decommission request accepted; the gateway will drain via the migration state machine."
  exit 0
fi

: "${CONSOLE_URL:?--console-url required}"
: "${ADMIN_TOKEN:?--admin-token required}"
: "${SPEC:?--spec required}"

if [ ! -f "$SPEC" ]; then
  echo "spec file not found: $SPEC" >&2
  exit 1
fi

tenant_id=$(jq -r .tenant_id "$SPEC")
region=$(jq -r .region "$SPEC")
country=$(jq -r .country "$SPEC")
capacity=$(jq -r .capacity_petabytes "$SPEC")
ec_profile=$(jq -r .ec_profile "$SPEC")

echo "==> step 1/7: validate"
[ -n "$tenant_id" ] || { echo "spec.tenant_id missing"; exit 1; }
case "$ec_profile" in
  6+2|8+3|10+4|12+4|16+4) ;;
  *) echo "spec.ec_profile must be one of 6+2 8+3 10+4 12+4 16+4"; exit 1;;
esac
echo "    OK tenant=$tenant_id region=$region country=$country capacity=${capacity}PB ec=$ec_profile"

echo "==> step 2/7: allocate hardware (terraform)"
echo "    NOTE: this would run 'terraform apply' against deploy/local-dc/terraform/."
echo "    For Phase 3 the operator runs this manually; the script logs the spec for audit."

echo "==> step 3/7: deploy ceph (ansible + cephadm)"
echo "    NOTE: this would run 'ansible-playbook' from deploy/local-dc/ansible/."

echo "==> step 4/7: register cell with console"
"$HERE/register_cell.sh" \
  --console-url "$CONSOLE_URL" \
  --admin-token "$ADMIN_TOKEN" \
  --tenant "$tenant_id" \
  --spec "$SPEC"

echo "==> step 5/7: apply tenant placement policy"
"$HERE/apply_placement.sh" \
  --console-url "$CONSOLE_URL" \
  --admin-token "$ADMIN_TOKEN" \
  --tenant "$tenant_id" \
  --provider ceph_rgw \
  --country "$country" \
  --ec-profile "$ec_profile"

echo "==> step 6/7: reload gateway fleet (SIGHUP)"
echo "    NOTE: run 'sudo systemctl reload zk-gateway' on each gateway node, one at a time."

echo "==> step 7/7: flip cell to active (after compliance verification)"
echo "    Run flip_active.sh once 'go test -v -run TestSuite_CephRGW ./tests/s3_compat/' is green"
echo "    against the new cell's RGW endpoint."

echo "Done."
