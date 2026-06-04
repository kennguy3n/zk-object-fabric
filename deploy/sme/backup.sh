#!/usr/bin/env bash
# Postgres logical backup for the SME single-pool deployment.
#
#   ./backup.sh
#
# Dumps the control-plane database with pg_dump (run inside the postgres
# container so no client install is needed on the host), compresses it,
# and uploads it to Wasabi under s3://$WASABI_BUCKET/backups/postgres/.
# Wasabi is S3-compatible, so the standard aws CLI works against it with
# --endpoint-url. Old backups beyond the retention window are pruned.
#
# Requires: docker compose, aws CLI. Reads credentials from the sibling
# .env (the same file the stack boots from).
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ENV_FILE="${ENV_FILE:-$SCRIPT_DIR/.env}"
COMPOSE_FILE="${COMPOSE_FILE:-$SCRIPT_DIR/docker-compose.production.yml}"
RETENTION_DAYS="${RETENTION_DAYS:-14}"

if [ ! -f "$ENV_FILE" ]; then
    echo "backup: $ENV_FILE not found; copy .env.example to .env first" >&2
    exit 1
fi

# Load .env without leaking values into the shell's xtrace.
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

: "${POSTGRES_USER:=postgres}"
: "${POSTGRES_DB:=zkof}"
: "${WASABI_BUCKET:?WASABI_BUCKET must be set in .env}"
: "${WASABI_ENDPOINT:?WASABI_ENDPOINT must be set in .env}"
: "${WASABI_REGION:?WASABI_REGION must be set in .env}"
: "${WASABI_ACCESS_KEY:?WASABI_ACCESS_KEY must be set in .env}"
: "${WASABI_SECRET_KEY:?WASABI_SECRET_KEY must be set in .env}"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT
dump_file="$workdir/zkof-$timestamp.sql.gz"

echo "backup: dumping database '$POSTGRES_DB' ..."
docker compose -f "$COMPOSE_FILE" exec -T postgres \
    pg_dump --clean --if-exists --no-owner --no-privileges \
    -U "$POSTGRES_USER" "$POSTGRES_DB" \
    | gzip -9 > "$dump_file"

size="$(wc -c < "$dump_file")"
if [ "$size" -eq 0 ]; then
    echo "backup: pg_dump produced an empty file; aborting" >&2
    exit 1
fi
echo "backup: wrote $dump_file ($size bytes)"

# Wasabi auth reuses the bucket credentials as AWS-style creds for this
# invocation only — exported into the environment, never written to disk.
export AWS_ACCESS_KEY_ID="$WASABI_ACCESS_KEY"
export AWS_SECRET_ACCESS_KEY="$WASABI_SECRET_KEY"
export AWS_DEFAULT_REGION="$WASABI_REGION"
endpoint="https://$WASABI_ENDPOINT"
key="backups/postgres/zkof-$timestamp.sql.gz"

echo "backup: uploading to s3://$WASABI_BUCKET/$key ..."
aws --endpoint-url "$endpoint" s3 cp "$dump_file" "s3://$WASABI_BUCKET/$key"

echo "backup: pruning backups older than ${RETENTION_DAYS}d ..."
cutoff="$(date -u -d "${RETENTION_DAYS} days ago" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -v-"${RETENTION_DAYS}"d +%Y-%m-%dT%H:%M:%SZ)"
aws --endpoint-url "$endpoint" s3api list-objects-v2 \
    --bucket "$WASABI_BUCKET" --prefix "backups/postgres/" \
    --query "Contents[?LastModified<='$cutoff'].Key" --output text 2>/dev/null \
    | tr '\t' '\n' \
    | while IFS= read -r old_key; do
        [ -n "$old_key" ] || continue
        echo "backup: removing s3://$WASABI_BUCKET/$old_key"
        aws --endpoint-url "$endpoint" s3 rm "s3://$WASABI_BUCKET/$old_key"
      done

echo "backup: done."
