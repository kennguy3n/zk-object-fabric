#!/usr/bin/env bash
# Bootstrap a Ceph Reef cluster on the current node via cephadm.
# Idempotent: rerunning on an already-bootstrapped node prints a
# warning and exits 0.
set -euo pipefail

CLUSTER_NAME="zkof-beta-cell-01"
PUBLIC_NETWORK=""
CLUSTER_NETWORK=""
RGW_REALM=""
RGW_ZONEGROUP=""
RGW_ZONE=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --cluster-name)    CLUSTER_NAME="$2"; shift 2;;
    --public-network)  PUBLIC_NETWORK="$2"; shift 2;;
    --cluster-network) CLUSTER_NETWORK="$2"; shift 2;;
    --rgw-realm)       RGW_REALM="$2"; shift 2;;
    --rgw-zonegroup)   RGW_ZONEGROUP="$2"; shift 2;;
    --rgw-zone)        RGW_ZONE="$2"; shift 2;;
    *) shift;;
  esac
done

if [ -d "/etc/ceph" ] && [ -f "/etc/ceph/ceph.conf" ]; then
  echo "Ceph cluster already bootstrapped at /etc/ceph; skipping."
  exit 0
fi

# Install cephadm and the Reef release shim.
curl -fsSL https://download.ceph.com/rpm-reef/el9/noarch/cephadm -o /usr/local/sbin/cephadm
chmod +x /usr/local/sbin/cephadm
cephadm add-repo --release reef
cephadm install ceph-common

# Pick the first global-scope IPv4 that falls inside PUBLIC_NETWORK
# (so multi-homed nodes with a separate cluster network don't pin the
# mon to the wrong interface). When PUBLIC_NETWORK is empty fall back
# to the first global-scope IPv4 on the host.
all_ips=$(ip -4 -j addr show | jq -r '.[] | .addr_info[] | select(.scope=="global") | .local')
if [ -n "$PUBLIC_NETWORK" ]; then
  mon_ip=$(printf '%s\n' "$all_ips" | python3 -c '
import ipaddress, sys
net = ipaddress.ip_network(sys.argv[1], strict=False)
for line in sys.stdin:
    ip = line.strip()
    if not ip:
        continue
    try:
        if ipaddress.ip_address(ip) in net:
            print(ip)
            break
    except ValueError:
        continue
' "$PUBLIC_NETWORK")
else
  mon_ip=$(printf '%s\n' "$all_ips" | head -1)
fi

if [ -z "$mon_ip" ]; then
  echo "no global-scope IPv4 address found inside ${PUBLIC_NETWORK:-<host>}" >&2
  exit 1
fi

cephadm bootstrap \
  --mon-ip "$mon_ip" \
  ${CLUSTER_NETWORK:+--cluster-network "$CLUSTER_NETWORK"} \
  --skip-monitoring-stack=false \
  --initial-dashboard-user zkof-admin \
  --allow-fqdn-hostname

# Create the RGW realm/zonegroup/zone if requested. cephadm will
# place RGW services in the cluster spec; the orchestrator handles
# subsequent expansion.
if [ -n "$RGW_REALM" ]; then
  cephadm shell -- radosgw-admin realm create --rgw-realm "$RGW_REALM" --default
  cephadm shell -- radosgw-admin zonegroup create --rgw-zonegroup "$RGW_ZONEGROUP" --master --default
  cephadm shell -- radosgw-admin zone create --rgw-zonegroup "$RGW_ZONEGROUP" --rgw-zone "$RGW_ZONE" --master --default
  cephadm shell -- radosgw-admin period update --commit
fi

echo
echo "Cluster $CLUSTER_NAME bootstrapped. Apply cluster.yaml next:"
echo "  cephadm shell -- ceph orch apply -i cephadm/cluster.yaml"
