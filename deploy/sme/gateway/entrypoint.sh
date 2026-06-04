#!/bin/sh
# Render the production gateway config from config.json.tmpl using the
# environment (Wasabi creds, Postgres DSN, ClickHouse URL, console
# secrets, …) and exec the gateway. envsubst ships in the runtime image
# (gettext) — the same pattern demo/entrypoint.sh uses for tenants.json,
# so secrets live in the environment / mounted key files and never in a
# committed config. The metadata DSN, JWT signing key, and manifest body
# key are all required for env=production; the gateway refuses to boot
# without them (see cmd/gateway production guards).
set -eu

TEMPLATE="${ZKOF_CONFIG_TEMPLATE:-/etc/zkof/config.json.tmpl}"
RENDERED="${ZKOF_CONFIG_RENDERED:-/run/sme/config.json}"

# Only substitute the variables we own. An explicit allow-list keeps
# envsubst from mangling any future literal "$" in the template and
# documents exactly which knobs the deployment injects. The single
# quotes are deliberate: envsubst must receive the literal variable
# names, not their values.
# shellcheck disable=SC2016
VARS='${ADMIN_TOKEN} ${ZKOF_CELL_ID}
${WASABI_ENDPOINT} ${WASABI_REGION} ${WASABI_BUCKET}
${WASABI_ACCESS_KEY} ${WASABI_SECRET_KEY}
${CLICKHOUSE_URL} ${CLICKHOUSE_DATABASE} ${CLICKHOUSE_USER} ${CLICKHOUSE_PASSWORD}
${METADATA_DSN} ${CONSOLE_HOST}
${KMS_CMK_URI} ${KMS_REGION} ${VAULT_ADDR} ${VAULT_TOKEN}'

mkdir -p "$(dirname "$RENDERED")"
envsubst "$VARS" < "$TEMPLATE" > "$RENDERED"

exec gateway -config "$RENDERED"
