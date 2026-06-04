#!/bin/sh
# One-shot bootstrap for the SME single-pool deployment. Idempotent:
# safe to re-run on every `docker compose up`. It performs the
# privileged, once-per-cell setup the gateway cannot do itself because
# in env=production it connects as a least-privilege, non-superuser role
# (see cmd/gateway checkProductionRLSRole):
#
#   1. Generate the console JWT signing key and the manifest body key
#      into the shared secrets volume (only if absent).
#   2. Provision the least-privilege Postgres role the gateway connects
#      as, apply every control-plane schema, create the manifests table,
#      arm Row-Level Security, and grant the role DML.
#   3. Create the ClickHouse billing database and apply its schema.
#
# Schemas are read from the repo checkout mounted read-only at /repo, so
# this script always tracks the in-tree DDL (the single source of truth)
# rather than a divergent copy.
set -eu

log() { printf '[bootstrap] %s\n' "$*"; }

REPO="${REPO_DIR:-/repo}"
SECRETS_DIR="${SECRETS_DIR:-/run/sme/secrets}"
BOOTSTRAP_DIR="${BOOTSTRAP_DIR:-/bootstrap}"

# ----------------------------------------------------------------------
# 0. Tooling. busybox already provides sh/wget/head; add psql + openssl.
# ----------------------------------------------------------------------
if ! command -v psql >/dev/null 2>&1 || ! command -v openssl >/dev/null 2>&1; then
    log "installing postgresql-client and openssl"
    apk add --no-cache postgresql-client openssl >/dev/null
fi

# ----------------------------------------------------------------------
# 1. Secrets. Generated once and persisted on the shared volume so every
#    gateway replica reads identical key material and it survives a
#    restart.
# ----------------------------------------------------------------------
mkdir -p "$SECRETS_DIR"

JWT_KEY="$SECRETS_DIR/jwt_signing_key.pem"
if [ ! -s "$JWT_KEY" ]; then
    log "generating console JWT signing key (RSA 2048, PKCS#8)"
    openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out "$JWT_KEY"
    chmod 600 "$JWT_KEY"
else
    log "console JWT signing key already present"
fi

MANIFEST_KEY="$SECRETS_DIR/manifest_body.key"
if [ ! -s "$MANIFEST_KEY" ]; then
    log "generating manifest body key (32 raw bytes, XChaCha20-Poly1305)"
    head -c 32 /dev/urandom > "$MANIFEST_KEY"
    chmod 600 "$MANIFEST_KEY"
else
    log "manifest body key already present"
fi

# ----------------------------------------------------------------------
# 2. Postgres. Connects as the superuser (POSTGRES_USER) to provision
#    the least-privilege gateway role and the schema it owns.
# ----------------------------------------------------------------------
export PGHOST="${POSTGRES_HOST:-postgres}"
export PGPORT="${POSTGRES_PORT:-5432}"
export PGUSER="${POSTGRES_USER:-postgres}"
export PGPASSWORD="${POSTGRES_PASSWORD:?POSTGRES_PASSWORD is required}"
export PGDATABASE="${POSTGRES_DB:-zkof}"
: "${ZKOF_APP_PASSWORD:?ZKOF_APP_PASSWORD is required}"

log "waiting for Postgres at $PGHOST:$PGPORT"
until pg_isready -q; do sleep 1; done

log "provisioning least-privilege role zkof_app"
psql -v ON_ERROR_STOP=1 -q \
    -v pw="$ZKOF_APP_PASSWORD" -v db="$PGDATABASE" <<'SQL'
SELECT 'CREATE ROLE zkof_app LOGIN NOSUPERUSER NOBYPASSRLS'
 WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'zkof_app')\gexec
ALTER ROLE zkof_app WITH LOGIN NOSUPERUSER NOBYPASSRLS PASSWORD :'pw';
GRANT CONNECT ON DATABASE :"db" TO zkof_app;
GRANT USAGE ON SCHEMA public TO zkof_app;
SQL

# Control-plane schemas (tables owned by the superuser). Order is not
# significant except that tenant_bindings references tenants — both live
# in internal/auth/schema.sql, so a single file keeps the FK satisfiable.
SCHEMAS="
internal/auth/schema.sql
internal/auth/legal_response_schema.sql
api/console/schema.sql
api/s3compat/multipart/schema.sql
metadata/bucket_config/schema.sql
metadata/content_index/schema.sql
internal/compliance/schema.sql
migration/coordination_schema.sql
"
# Apply tolerantly (ON_ERROR_STOP unset) so the bootstrap stays
# idempotent across the re-runs `docker compose up` triggers. Not every
# in-tree schema guards with IF NOT EXISTS (e.g.
# metadata/content_index/schema.sql does a bare CREATE TABLE/INDEX), so a
# second run would otherwise abort on "already exists". Genuine failures
# are still caught by the table-existence verification below rather than
# by aborting mid-file.
for rel in $SCHEMAS; do
    log "applying schema $rel"
    psql -q -f "$REPO/$rel" 2>&1 | grep -vE 'NOTICE|already exists, skipping' || true
done

log "applying manifests table"
psql -q -f "$BOOTSTRAP_DIR/00-manifests.sql" 2>&1 | grep -vE 'NOTICE' || true

# Verify every table the gateway depends on actually exists. This turns
# the tolerant apply above into a hard gate: a schema that genuinely
# failed to create its table (as opposed to one that already existed)
# leaves a NULL here and fails the bootstrap loudly.
log "verifying control-plane tables"
missing="$(psql -v ON_ERROR_STOP=1 -tAc "
SELECT t FROM (VALUES
  ('tenants'),('tenant_bindings'),('legal_holds'),
  ('placement_policies'),('auth_users'),('refresh_tokens'),
  ('mfa_credentials'),('mfa_recovery_codes'),('dedicated_cells'),
  ('multipart_uploads'),('multipart_parts'),
  ('bucket_versioning'),('bucket_object_lock'),('bucket_cors'),
  ('bucket_lifecycle'),('bucket_notification'),('bucket_encryption'),
  ('content_index'),('compliance_audit'),('tenant_country_allowlist'),
  ('migration_jobs'),('manifests')
) AS x(t)
WHERE to_regclass('public.' || t) IS NULL;")"
if [ -n "$missing" ]; then
    log "FATAL: expected tables missing after schema apply:"
    # Word-splitting is intentional: $missing is a newline-separated list
    # of bare table identifiers (no spaces/globs), one per printf cycle.
    # shellcheck disable=SC2086
    printf '  - %s\n' $missing >&2
    exit 1
fi

# Grant the gateway role DML on everything created above BEFORE arming
# RLS, so the per-table GRANTs in the rls.sql files layer on top.
log "granting DML to zkof_app"
psql -v ON_ERROR_STOP=1 -q <<'SQL'
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO zkof_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO zkof_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO zkof_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO zkof_app;
SQL

# Arm Row-Level Security on every tenant-scoped table. These files are
# the operator reference shipped beside each store and grant DML to
# zkof_app for their table.
RLS="
metadata/manifest_store/postgres/rls.sql
metadata/content_index/postgres/rls.sql
metadata/bucket_config/postgres/rls.sql
api/s3compat/multipart/rls.sql
"
for rel in $RLS; do
    log "arming RLS $rel"
    psql -v ON_ERROR_STOP=1 -q -f "$REPO/$rel"
done

# ----------------------------------------------------------------------
# 3. ClickHouse billing schema, applied over the HTTP interface (8123).
# ----------------------------------------------------------------------
CH_HOST="${CLICKHOUSE_HOST:-clickhouse}"
CH_PORT="${CLICKHOUSE_HTTP_PORT:-8123}"
CH_DB="${CLICKHOUSE_DATABASE:-zkof_billing}"
# Default to zkof (the compose-provisioned ClickHouse user), matching
# docker-compose.production.yml so a standalone run doesn't silently fall
# back to the built-in 'default' account and fail auth.
CH_USER="${CLICKHOUSE_USER:-zkof}"
CH_PASSWORD="${CLICKHOUSE_PASSWORD:-}"
CH_URL="http://$CH_HOST:$CH_PORT/"

# $CH_DB is interpolated into both a CREATE DATABASE statement and a sed
# replacement (s/{{database}}/$CH_DB/g) below. A value containing sed
# metacharacters (/ & \) or SQL would corrupt the rendered schema or worse.
# Restrict it to a plain ClickHouse identifier and fail loudly rather than
# silently producing a broken schema.
if ! printf '%s' "$CH_DB" | grep -qE '^[A-Za-z_][A-Za-z0-9_]*$'; then
    log "FATAL: CLICKHOUSE_DATABASE='$CH_DB' is not a valid identifier (^[A-Za-z_][A-Za-z0-9_]*\$)"
    exit 1
fi

ch_exec() {
    # $1 = SQL text. Authenticates via headers so the password never
    # lands in a URL/argv that shows up in process listings or logs.
    wget -q -O - \
        --header="X-ClickHouse-User: $CH_USER" \
        --header="X-ClickHouse-Key: $CH_PASSWORD" \
        --post-data="$1" "$CH_URL" >/dev/null
}

log "waiting for ClickHouse at $CH_HOST:$CH_PORT"
until wget -q -O - "$CH_URL" --post-data="SELECT 1" \
    --header="X-ClickHouse-User: $CH_USER" \
    --header="X-ClickHouse-Key: $CH_PASSWORD" >/dev/null 2>&1; do
    sleep 1
done

log "creating ClickHouse database $CH_DB and applying billing schema"
ch_exec "CREATE DATABASE IF NOT EXISTS $CH_DB"
# Render {{database}} and apply the schema. The HTTP interface runs one
# statement per request, so split the file on ';' and POST each in turn.
# SQL line comments (--) are stripped FIRST: the schema's header comment
# contains a literal ';' ("INSERTs into usage_events; the aggregated …"),
# which would otherwise split off a comment-only fragment that ClickHouse
# rejects with HTTP 400 (empty query). Stripping comments also keeps the
# split robust against any future ';' inside a comment.
TMP_SQL="$(mktemp -d)"
trap 'rm -rf "$TMP_SQL"' EXIT
sed "s/{{database}}/$CH_DB/g" "$REPO/billing/schema.sql" \
    | sed 's/--.*$//' \
    | awk -v dir="$TMP_SQL" '
        BEGIN { RS = ";"; n = 0 }
        /[^[:space:]]/ { n++; f = sprintf("%s/%04d.sql", dir, n); print $0 > f; close(f) }
      '
for stmt_file in "$TMP_SQL"/*.sql; do
    [ -e "$stmt_file" ] || continue
    ch_exec "$(cat "$stmt_file")"
done

log "bootstrap complete"
