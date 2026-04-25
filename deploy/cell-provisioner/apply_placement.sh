#!/usr/bin/env bash
# PUT /api/tenants/{id}/placement against the console API.
set -euo pipefail

CONSOLE_URL=""
ADMIN_TOKEN=""
TENANT=""
PROVIDER=""
COUNTRY=""
EC_PROFILE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --console-url) CONSOLE_URL="$2"; shift 2;;
    --admin-token) ADMIN_TOKEN="$2"; shift 2;;
    --tenant)      TENANT="$2"; shift 2;;
    --provider)    PROVIDER="$2"; shift 2;;
    --country)     COUNTRY="$2"; shift 2;;
    --ec-profile)  EC_PROFILE="$2"; shift 2;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

body=$(jq -nc \
  --arg provider "$PROVIDER" \
  --arg country  "$COUNTRY" \
  --arg ec       "$EC_PROFILE" \
  '{
    provider:      $provider,
    country_allow_list: [$country],
    erasure_profile: $ec,
    encryption_mode: "managed"
  }')

curl -fsS -X PUT \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data "$body" \
  "$CONSOLE_URL/api/tenants/$TENANT/placement"
