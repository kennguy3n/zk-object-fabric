#!/usr/bin/env bash
# Idempotent installer for the zk-gateway binary on a fresh Linode
# instance. Cloud-init invokes this at first boot. Re-running it on
# a live host is safe; it only restarts the service if the binary
# checksum changes.
set -euo pipefail

RELEASE_URL="${RELEASE_URL:?set to the signed gateway tarball URL}"
INSTALL_DIR="/opt/zk-gateway/bin"
CACHE_DIR="/var/lib/zkof/cache"

install -d "$INSTALL_DIR" "$CACHE_DIR" /etc/zk-gateway

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT

curl -fsSL "$RELEASE_URL" -o "$tmp"

new_sum=$(sha256sum "$tmp" | awk '{print $1}')
old_sum=""
if [ -f "$INSTALL_DIR/gateway.sha256" ]; then
  old_sum=$(cat "$INSTALL_DIR/gateway.sha256")
fi

if [ "$new_sum" != "$old_sum" ]; then
  tar -xzf "$tmp" -C "$INSTALL_DIR"
  echo "$new_sum" > "$INSTALL_DIR/gateway.sha256"
  systemctl restart zk-gateway || systemctl start zk-gateway
fi

if ! id -u zkof >/dev/null 2>&1; then
  useradd --system --home /var/lib/zkof --shell /usr/sbin/nologin zkof
fi
chown -R zkof:zkof /var/lib/zkof /etc/zk-gateway
