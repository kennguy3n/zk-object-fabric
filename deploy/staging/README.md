# Tier 3 (Linode + Wasabi) staging deployment

This directory holds the operator-runnable
artifacts that stand up the published-SLA staging environment
described in `docs/runbooks/load-testing.md` §4 ("Tier 3: Linode +
Wasabi staging gateway"). Running the harness here produces the
canonical benchmark report that demonstrates the gateway meets its
published load-testing SLA on a real cloud deployment.

The contents are **external-prep-only** — every script in this tree
is designed to be invoked by a human operator (or an external party
to whom you delegate the staging run). Nothing in `deploy/staging`
is wired into automated CI, because the run consumes paid Linode
compute and paid Wasabi storage; the run cadence is one staging
shakedown per release candidate, controlled by the release manager.

## Reading order

1. **This README** for the overall topology and the ordered runbook.
2. [`load-driver/terraform/`](load-driver/terraform/) for the
   Terraform module that spins up the load-driver VM in the same
   region as the gateway fleet.
3. [`load-driver/scripts/run_tier3.sh`](load-driver/scripts/run_tier3.sh)
   for the end-to-end execution shell script that runs on the load
   driver.
4. [`scripts/collect_evidence.sh`](scripts/collect_evidence.sh) for
   the dossier-assembly script that pulls every artifact into a
   single signed tarball ready for the audit dossier
   (`make tier3-evidence`).
5. [`evidence/README.md`](evidence/README.md) for the layout
   contract the dossier directory adheres to.
6. The companion runbook
   [`docs/runbooks/load-testing.md`](../../docs/runbooks/load-testing.md)
   §4 for the **acceptance criteria** the run is gated on.

## Topology

```
+----------------------------------------------------------------+
|                        Tier 3 staging                          |
|                                                                |
|    +-----------------+         +---------------------------+   |
|    |  load driver    |  HTTPS  |   gateway fleet (3x)      |   |
|    |  (Linode VM,    | ------> |   Linode dedicated CPU    |   |
|    |   benchmark-    |         |   8 vCPU / 16 GB RAM /    |   |
|    |   runner +      |         |   500 GB NVMe cache       |   |
|    |   tier3-verify) |         |   behind Linode           |   |
|    +-----------------+         |   NodeBalancer            |   |
|             |                  +---------------------------+   |
|             |                              |                   |
|             |                              v                   |
|             |                  +---------------------------+   |
|             +----------------> |  Wasabi origin            |   |
|                evidence /      |  one bucket per region    |   |
|                logs            |  (zkof-<region>-staging)  |   |
|                                +---------------------------+   |
+----------------------------------------------------------------+
```

The load driver and the gateway fleet **must** be in the same
Linode region so the measured latencies reflect the production
gateway-to-client path rather than transcontinental RTT. The
canonical region for the published SLA run is `us-east`.

## Prerequisites checklist

Before running the staging pipeline, confirm each of the following:

- [ ] **Linode API token** with full account access exported as
      `LINODE_TOKEN`. The deploy uses `terraform/main.tf`'s
      `linode_token` variable, which is sensitive.
- [ ] **Linode SSH key** uploaded; its public key path exported as
      `SSH_PUBLIC_KEY_FILE` (defaults to `~/.ssh/id_ed25519.pub`).
      Used by both gateway Terraform and load-driver Terraform.
- [ ] **Wasabi root credentials** exported as
      `WASABI_ROOT_ACCESS_KEY` / `WASABI_ROOT_SECRET_KEY`. Used to
      provision the staging buckets via
      `deploy/wasabi/provision_buckets.sh`.
- [ ] **Per-region Wasabi IAM users created** with the
      `iam_policy.template.json` policy applied. The gateway uses
      these keys (not the root keys) at runtime. Export the
      staging credentials as `WASABI_STAGING_ACCESS_KEY` /
      `WASABI_STAGING_SECRET_KEY` for `run_tier3.sh`.
- [ ] **Signed gateway tarball** (`gateway-<sha>.tar.gz`) published
      to a URL the Linode instances can fetch over HTTPS. Export
      that URL as `GATEWAY_RELEASE_URL`. The release pipeline
      produces this artifact; for an ad-hoc rebuild, the dossier
      capture step records the exact SHA.
- [ ] **Wasabi bucket logging enabled** on the staging bucket. The
      bucket access logs are part of the audit dossier and must be
      written into a separate bucket
      (`zkof-<region>-staging-logs`) before the run starts;
      otherwise the dossier is incomplete.

## Runbook (5 stages)

The 5 stages produce a self-contained evidence directory
(`deploy/staging/evidence/<UTC-timestamp>-<gateway-sha>/`) suitable
for the audit dossier.

### Stage 1 — Wasabi buckets

```bash
cd deploy/wasabi
cp regions.env.example regions.env
$EDITOR regions.env              # set ZKOF_ENV=staging and the regions you want
./provision_buckets.sh           # idempotent; safe to re-run
```

Confirm `gateway_config.generated.json` was written. Replace each
`REPLACE-WITH-PER-REGION-IAM-USER-AK` placeholder with the IAM-user
access key for that region's bucket (the placeholders are
intentional — the script does not have those credentials).

### Stage 2 — Gateway fleet

```bash
cd deploy/linode/terraform
terraform init
terraform workspace new staging-us-east-1
terraform apply \
  -var "linode_token=$LINODE_TOKEN" \
  -var "region=us-east" \
  -var "env=staging" \
  -var "fleet_size=3" \
  -var "instance_type=g6-dedicated-8" \
  -var "ssh_authorized_keys=[$(awk '{printf "\"%s\"", $0}' "${SSH_PUBLIC_KEY_FILE:-$HOME/.ssh/id_ed25519.pub}")]" \
  -var "gateway_release_url=$GATEWAY_RELEASE_URL"
```

The fleet boots, cloud-init runs `install_gateway.sh`, Caddy comes
up with TLS, and the NodeBalancer health-probes `/internal/ready`.
Wait for all three nodes to report ready
(`./deploy/linode/scripts/health_check.sh` on each).

### Stage 3 — Load driver

```bash
cd deploy/staging/load-driver/terraform
terraform init
terraform workspace new staging-us-east-1
terraform apply \
  -var "linode_token=$LINODE_TOKEN" \
  -var "region=us-east" \
  -var "env=staging" \
  -var "ssh_authorized_keys=[$(awk '{printf "\"%s\"", $0}' "${SSH_PUBLIC_KEY_FILE:-$HOME/.ssh/id_ed25519.pub}")]" \
  -var "benchmark_release_url=$BENCHMARK_RELEASE_URL"
```

The load driver is a single Linode VM (smaller than the gateway —
`g6-standard-4` is sufficient) provisioned with the `benchmark-runner`
binary and the `tier3-verify` binary. It must live in the same
Linode region as the gateway fleet so the measured latency reflects
intra-region RTT.

### Stage 4 — Run the harness

SSH to the load driver and run the end-to-end script:

```bash
ssh ubuntu@$(terraform -chdir=deploy/staging/load-driver/terraform output -raw load_driver_ip)

# On the load driver:
export STAGING_BUCKET=zkof-us-east-1-staging
export WASABI_KEY=...
export WASABI_SECRET=...
export GATEWAY_ENDPOINT=https://<nodebalancer-hostname>
export GATEWAY_SHA=$(curl -s "$GATEWAY_ENDPOINT/internal/version" | jq -r .commit)

./run_tier3.sh
```

`run_tier3.sh` runs the canonical staging invocation
(`-duration=1h -rps=12000 -concurrency=128 -seed-objects=10000`)
against the gateway, writes the report JSON to
`reports/linode-wasabi-<timestamp>.json`, invokes
`tier3-verify -report <report>` to produce
`reports/linode-wasabi-<timestamp>.verdict.json`, and exits non-zero
on verifier failure. The script does **not** retry on failure —
the operator is expected to triage and re-run manually after
investigating.

### Stage 5 — Collect evidence

Back on the operator workstation:

```bash
export GATEWAY_NODES="<nb-host-1> <nb-host-2> <nb-host-3>"
export LOAD_DRIVER_HOST=<load-driver-host>
export STAGING_BUCKET=zkof-us-east-1-staging

./deploy/staging/scripts/collect_evidence.sh
```

[`scripts/collect_evidence.sh`](scripts/collect_evidence.sh):

1. Downloads `reports/*.json` and `reports/*.verdict.json` from
   the load driver.
2. Downloads `journalctl -u zk-gateway` from each gateway node
   covering the run window.
3. Downloads the Wasabi bucket access logs for the run window.
4. Captures the NodeBalancer health check history via the Linode
   API.
5. Captures the gateway `/internal/health` snapshot and the
   Prometheus scrape from each node.
6. Writes an `00-environment.json` file describing the
   gateway-sha, terraform workspace, load-driver-sha, and the
   region.
7. Assembles all of the above into
   `deploy/staging/evidence/<UTC-timestamp>-<gateway-sha>/` and
   produces a `MANIFEST.txt` with SHA-256 hashes per file plus a
   top-level `tier3-evidence-<timestamp>-<sha>.tar.gz` for
   shipping to the audit dossier.

## Acceptance criteria

The verdict JSON produced in Stage 4 implements the load-testing
runbook's §4 contract:

| # | Criterion | Where enforced |
|---|---|---|
| 1 | `Report.AllPassed == true`. | `cmd/tier3-verify/main.go` (re-applies the per-metric thresholds independently of this flag). |
| 2 | All 6 Tier 3 scenarios present (put-cache-hit, put-origin, get-l0-cache-hit, get-l1-cache-hit, get-origin, sustained-throughput-10k-rps). | `tests/tier3verify.RequiredScenarios`. |
| 3 | Every latency `*_p99` ≤ its `Target*Ms` constant. | `tests/tier3verify.Tier3Thresholds`. |
| 4 | `sustained_rps >= 10_000` ∧ `rps_efficiency >= 0.95` ∧ `error_rate <= 1e-3` ∧ `skipped_op_fraction <= 0.05`. | Same. |
| 5 | No required metric `Pending`. | `tests/tier3verify.verifyScenario`. |

A `tier3-verify` exit code of 0 means the run satisfies the
published load-testing SLA. A non-zero exit means **do
NOT promote** the gateway build — file an incident, attach the
verdict and the full evidence dossier, and triage from the
per-scenario `histogram` field in the JSON.

## Teardown

```bash
terraform -chdir=deploy/staging/load-driver/terraform destroy ...
terraform -chdir=deploy/linode/terraform destroy ...
# Leave the Wasabi buckets in place — the staging access logs in
# zkof-<region>-staging-logs are part of the audit dossier and
# must be retained for at least the audit retention window.
```

## See also

- [`docs/runbooks/load-testing.md`](../../docs/runbooks/load-testing.md)
  §4 — Tier 3 invocation contract and load-testing SLA targets.
- [`tests/tier3verify`](../../tests/tier3verify) — the verifier
  package consumed by the CLI.
- [`cmd/tier3-verify`](../../cmd/tier3-verify) — the CLI itself.
