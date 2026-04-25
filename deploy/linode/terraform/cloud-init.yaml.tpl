#cloud-config
package_update: true
package_upgrade: true

packages:
  - curl
  - jq
  - ca-certificates

write_files:
  - path: /etc/zk-gateway/install_gateway.sh
    permissions: "0755"
    content: |
      #!/usr/bin/env bash
      set -euo pipefail
      RELEASE_URL="${gateway_release_url}"
      ENV="${env}"
      REGION="${region}"
      curl -fsSL "$RELEASE_URL" -o /tmp/zk-gateway.tar.gz
      install -d /opt/zk-gateway/bin /var/lib/zkof/cache /etc/zk-gateway
      tar -xzf /tmp/zk-gateway.tar.gz -C /opt/zk-gateway/bin
      id -u zkof >/dev/null 2>&1 || useradd --system --home /var/lib/zkof --shell /usr/sbin/nologin zkof
      chown -R zkof:zkof /var/lib/zkof /etc/zk-gateway
      cp /etc/zk-gateway/zk-gateway.service /etc/systemd/system/
      systemctl daemon-reload
      systemctl enable --now zk-gateway

runcmd:
  - /etc/zk-gateway/install_gateway.sh
