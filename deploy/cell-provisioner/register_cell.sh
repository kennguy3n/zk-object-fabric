#!/usr/bin/env bash
# POST /api/tenants/{id}/dedicated-cells against the console API.
set -euo pipefail

CONSOLE_URL=""
ADMIN_TOKEN=""
TENANT=""
SPEC=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --console-url) CONSOLE_URL="$2"; shift 2;;
    --admin-token) ADMIN_TOKEN="$2"; shift 2;;
    --tenant)      TENANT="$2"; shift 2;;
    --spec)        SPEC="$2"; shift 2;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

: "${CONSOLE_URL:?--console-url required}"
: "${ADMIN_TOKEN:?--admin-token required}"
: "${TENANT:?--tenant required}"
: "${SPEC:?--spec required}"

body=$(jq -c '{
  region:             .region,
  country:            .country,
  capacity_petabytes: .capacity_petabytes,
  ec_profile:         .ec_profile,
  notes:              .notes
}' "$SPEC")

resp=$(curl -fsS -X POST \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data "$body" \
  "$CONSOLE_URL/api/tenants/$TENANT/dedicated-cells")

echo "$resp" | jq .
cell_id=$(echo "$resp" | jq -r .id)
echo "ZKOF_CELL_ID=$cell_id"
