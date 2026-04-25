#!/usr/bin/env bash
# PATCH /api/admin/dedicated-cells/{cell_id} to flip status to
# active. Phase 4 endpoint — Phase 3 ships the provisioning +
# decommission paths only. This wrapper exists so operator
# muscle memory survives the Phase 4 cutover.
set -euo pipefail

CONSOLE_URL=""
ADMIN_TOKEN=""
CELL=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --console-url) CONSOLE_URL="$2"; shift 2;;
    --admin-token) ADMIN_TOKEN="$2"; shift 2;;
    --cell)        CELL="$2"; shift 2;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

curl -fsS -X PATCH \
  -H "Authorization: Bearer $ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"status":"active"}' \
  "$CONSOLE_URL/api/admin/dedicated-cells/$CELL"
