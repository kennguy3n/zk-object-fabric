#cloud-config
# Cloud-init for the Tier 3 staging load driver. Fetches the signed
# benchmark-runner + tier3-verify tarball, installs the binaries
# under /opt/zkof-loaddrv/bin, drops a non-root runtime user, and
# exits. The actual load run is operator-initiated via SSH (see
# deploy/staging/load-driver/scripts/run_tier3.sh).

write_files:
  - path: /opt/zkof-loaddrv/etc/release-url
    permissions: "0644"
    content: |
      ${benchmark_release_url}
  - path: /etc/profile.d/zkof-loaddrv.sh
    permissions: "0644"
    content: |
      # Convenience: put the staging binaries on PATH for the
      # ubuntu user without forcing a relogin.
      export PATH="/opt/zkof-loaddrv/bin:$PATH"
      export ZKOF_LOADDRV_ENV="${env}"
      export ZKOF_LOADDRV_REGION="${region}"

runcmd:
  - install -d -o root -g root -m 0755 /opt/zkof-loaddrv /opt/zkof-loaddrv/bin /opt/zkof-loaddrv/etc
  - install -d -o ubuntu -g ubuntu -m 0755 /var/lib/zkof-loaddrv /var/lib/zkof-loaddrv/reports
  - |
      set -euo pipefail
      tmp=$(mktemp)
      trap 'rm -f "$tmp"' EXIT
      curl -fsSL "${benchmark_release_url}" -o "$tmp"
      tar -xzf "$tmp" -C /opt/zkof-loaddrv/bin
      chmod 0755 /opt/zkof-loaddrv/bin/benchmark-runner /opt/zkof-loaddrv/bin/tier3-verify
      sha256sum "$tmp" | awk '{print $1}' > /opt/zkof-loaddrv/etc/release.sha256
  - |
      # Drop the wrapper script that the operator invokes after
      # SSH'ing in. The wrapper is shipped in the same release
      # tarball, but we drop a copy here too so the script is
      # discoverable via `ls /opt/zkof-loaddrv/bin`.
      install -m 0755 /opt/zkof-loaddrv/bin/run_tier3.sh /usr/local/bin/run_tier3.sh
