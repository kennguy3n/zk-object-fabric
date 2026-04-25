#!/usr/bin/env bash
# Idempotent Wasabi bucket provisioner for the Phase 3 multi-region
# origin. Reads regions.env (sourced) and creates one bucket per
# region named zkof-${REGION}-${ZKOF_ENV} with the per-bucket IAM
# policy and CORS configuration.
#
# Requires: aws CLI v2, jq.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -f "$HERE/regions.env" ]; then
  echo "deploy/wasabi: regions.env not found; copy regions.env.example and edit it." >&2
  exit 1
fi

# shellcheck disable=SC1091
source "$HERE/regions.env"

: "${WASABI_ROOT_ACCESS_KEY:?set in regions.env}"
: "${WASABI_ROOT_SECRET_KEY:?set in regions.env}"
: "${ZKOF_ENV:?set in regions.env}"
: "${WASABI_REGIONS:?set in regions.env}"

POLICY_TEMPLATE="$HERE/iam_policy.template.json"
CORS_CONFIG="$HERE/cors_config.json"

GATEWAY_CONFIG_OUT="$HERE/gateway_config.generated.json"
echo '{"providers":{"wasabi":{"regions":[' > "$GATEWAY_CONFIG_OUT.tmp"

first=1
while IFS= read -r line; do
  [ -z "$line" ] && continue
  region="${line%%:*}"
  endpoint="${line#*:}"
  bucket="zkof-${region}-${ZKOF_ENV}"

  echo "==> region=${region} bucket=${bucket} endpoint=${endpoint}"

  AWS_ACCESS_KEY_ID="$WASABI_ROOT_ACCESS_KEY" \
  AWS_SECRET_ACCESS_KEY="$WASABI_ROOT_SECRET_KEY" \
  AWS_DEFAULT_REGION="$region" \
    aws --endpoint-url "$endpoint" s3api create-bucket \
        --bucket "$bucket" 2>/dev/null || true

  AWS_ACCESS_KEY_ID="$WASABI_ROOT_ACCESS_KEY" \
  AWS_SECRET_ACCESS_KEY="$WASABI_ROOT_SECRET_KEY" \
  AWS_DEFAULT_REGION="$region" \
    aws --endpoint-url "$endpoint" s3api put-bucket-cors \
        --bucket "$bucket" \
        --cors-configuration "file://$CORS_CONFIG" >/dev/null

  AWS_ACCESS_KEY_ID="$WASABI_ROOT_ACCESS_KEY" \
  AWS_SECRET_ACCESS_KEY="$WASABI_ROOT_SECRET_KEY" \
  AWS_DEFAULT_REGION="$region" \
    aws --endpoint-url "$endpoint" s3api put-public-access-block \
        --bucket "$bucket" \
        --public-access-block-configuration \
        BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true \
        >/dev/null || true

  policy_file=$(mktemp)
  sed -e "s|@@BUCKET@@|${bucket}|g" "$POLICY_TEMPLATE" > "$policy_file"
  echo "    policy template written to $policy_file (apply via Wasabi console / IAM API)"

  if [ "$first" -eq 0 ]; then
    echo ',' >> "$GATEWAY_CONFIG_OUT.tmp"
  fi
  first=0
  cat >> "$GATEWAY_CONFIG_OUT.tmp" <<EOF
{
  "name":      "wasabi-${region}",
  "endpoint":  "${endpoint}",
  "region":    "${region}",
  "bucket":    "${bucket}",
  "access_key": "REPLACE-WITH-PER-REGION-IAM-USER-AK",
  "secret_key": "REPLACE-WITH-PER-REGION-IAM-USER-SK"
}
EOF
done <<< "$WASABI_REGIONS"

echo ']}}}' >> "$GATEWAY_CONFIG_OUT.tmp"
jq . "$GATEWAY_CONFIG_OUT.tmp" > "$GATEWAY_CONFIG_OUT"
rm -f "$GATEWAY_CONFIG_OUT.tmp"

echo
echo "==> All regions provisioned. Gateway config snippet: $GATEWAY_CONFIG_OUT"
echo "    Replace the per-region access_key / secret_key placeholders before"
echo "    rolling the config to production gateways."
