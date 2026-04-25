#!/usr/bin/env bash
# Local health probe for the gateway. Returns 0 (healthy) or
# nonzero (unhealthy). NodeBalancer is configured to call
# /internal/ready directly; this script is for on-host debugging
# and the systemd watchdog hook.
set -euo pipefail

curl -fsS --max-time 3 http://127.0.0.1:8080/internal/ready >/dev/null
echo "ok"
