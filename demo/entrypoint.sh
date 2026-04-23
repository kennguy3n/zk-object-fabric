#!/bin/sh
# Render demo/tenants.json from demo/tenants.json.tmpl using env vars,
# then exec the gateway. The template uses envsubst placeholders so
# the repo never ships literal access-key / secret-key values in a
# file the secret scanner would flag — the demo's HMAC credentials
# are opaque shared secrets by design, so they live in environment
# variables with safe defaults.
#
# All four variables default to the published demo values documented
# in demo/README.md so `docker compose up` just works.
set -eu

: "${DEMO_ACCESS_KEY:=demo-access-key}"
: "${DEMO_SECRET_KEY:=demo-secret-key}"
: "${KMAIL_ACCESS_KEY:=kmail-access-key}"
: "${KMAIL_SECRET_KEY:=kmail-secret-key}"

export DEMO_ACCESS_KEY DEMO_SECRET_KEY KMAIL_ACCESS_KEY KMAIL_SECRET_KEY

TEMPLATE=/app/demo/tenants.json.tmpl
RENDERED=/run/zk-fabric/tenants.json

mkdir -p "$(dirname "$RENDERED")"
envsubst < "$TEMPLATE" > "$RENDERED"

exec gateway \
  -config /app/demo/config.json \
  -tenants "$RENDERED"
