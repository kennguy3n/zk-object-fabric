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

# RFC 3986 percent-encode of $1, leaving unreserved characters intact. Pure
# POSIX/busybox: walk the string one character at a time and emit %XX for
# anything that is not unreserved. The leading-quote form of printf ("'$c")
# yields the numeric byte value of the character.
#
# ASCII only: for a multi-byte UTF-8 char, printf "'$c" returns the Unicode
# code point rather than the per-byte values, so this would not produce a
# correct encoding. That's fine here — the only input is ZKOF_APP_PASSWORD,
# which .env.example mandates be generated with `openssl rand -hex` (ASCII).
urlencode() {
    _ue_s="$1"
    while [ -n "$_ue_s" ]; do
        _ue_c="${_ue_s%"${_ue_s#?}"}"
        case "$_ue_c" in
            [a-zA-Z0-9.~_-]) printf '%s' "$_ue_c" ;;
            *) printf '%%%02X' "'$_ue_c" ;;
        esac
        _ue_s="${_ue_s#?}"
    done
}

# Build the Postgres control-plane DSN from its parts unless the operator
# pre-set METADATA_DSN (e.g. to point at an external/managed Postgres).
# The password is percent-encoded so a value containing URL-reserved
# characters (@ : / ? # %) round-trips through lib/pq's URL parser rather
# than being misparsed as a user-info/host boundary. URL form (vs lib/pq's
# keyword=value form) keeps the rendered value free of spaces, quotes and
# backslashes, so it stays valid inside the JSON config string envsubst
# produces below. Host/port/db match the single-pool compose topology and
# are overridable for non-default layouts.
if [ -z "${METADATA_DSN:-}" ]; then
    : "${ZKOF_APP_PASSWORD:?ZKOF_APP_PASSWORD is required to build METADATA_DSN}"
    _enc_pw="$(urlencode "$ZKOF_APP_PASSWORD")"
    METADATA_DSN="postgres://${ZKOF_DB_USER:-zkof_app}:${_enc_pw}@${ZKOF_DB_HOST:-postgres}:${ZKOF_DB_PORT:-5432}/${ZKOF_DB_NAME:-${POSTGRES_DB:-zkof}}?sslmode=${ZKOF_DB_SSLMODE:-disable}"
    export METADATA_DSN
fi

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
