#!/usr/bin/env bash
# PATCH /api/admin/dedicated-cells/{cell_id} to flip status to
# active. This manual status-flip endpoint is not part of the
# console's current dedicated-cell HTTP surface (which exposes the
# provisioning and decommission paths); cells advance to active
# through the provisioner's bring-up completion workflow. This
# wrapper is kept so operator muscle memory survives if a manual
# flip endpoint is added.
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
