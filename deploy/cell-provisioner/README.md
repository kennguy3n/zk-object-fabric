# B2B Dedicated Cell Provisioner

Operator scripts that drive the end-to-end provisioning flow for a
B2B / sovereign dedicated cell. The flow is:

1. Validate the request (region, country, capacity, EC profile).
2. Allocate hardware (or cloud resources via Terraform).
3. Deploy a Ceph RGW cluster for the cell (via
   [`deploy/local-dc/`](../local-dc/)).
4. Register the cell with the gateway's `DedicatedCellStore` via
   `POST /api/tenants/{id}/dedicated-cells`.
5. Apply the tenant's placement policy pointing at the new cell.
6. Hot-load the new `ceph_rgw` provider into the gateway's
   provider registry.
7. Flip the cell from `provisioning → active` once compliance
   tests pass.

## Layout

| File                            | Purpose                                                                  |
| ------------------------------- | ------------------------------------------------------------------------ |
| `provision_cell.sh`             | Top-level entry point. Reads `cell.spec.json`, runs steps 1-7.           |
| `cell.spec.example.json`        | Template request: tenant ID, region, country, capacity, EC profile.      |
| `register_cell.sh`              | Wrapper around `POST /api/tenants/{id}/dedicated-cells` (step 4).        |
| `apply_placement.sh`            | Wrapper around `PUT /api/tenants/{id}/placement` (step 5).               |
| `flip_active.sh`                | Marks the cell `active` once compliance has been verified (step 7).      |

## Quick start

```bash
cp cell.spec.example.json cell.spec.json
$EDITOR cell.spec.json     # set tenant_id, region, country, capacity, ec_profile

./provision_cell.sh \
  --console-url https://console.zkof.example \
  --admin-token "$ZKOF_ADMIN_TOKEN" \
  --spec ./cell.spec.json
```

The script is **non-destructive** through step 6: if step 7
(`flip_active.sh`) is interrupted, re-running `provision_cell.sh`
picks up where it left off. Hardware allocation (step 2) is the
only step that costs real money on a re-run, so the script
short-circuits when the Terraform state already shows the cell's
resources are present.

## Console-API integration

`register_cell.sh` posts the JSON body the
[`POST /api/tenants/{id}/dedicated-cells`](../../api/console/handler.go)
endpoint expects:

```json
{
  "region":             "local-dc-01",
  "country":            "US",
  "capacity_petabytes": 0.3,
  "ec_profile":         "6+2",
  "notes":              "Beta cell for tenant acme-co"
}
```

The endpoint validates the body, mints a fresh cell ID, persists
a `provisioning` record via the
`internal/cellops/provisioner.go#ManualProvisioner`, and returns
`202 Accepted` with the
[`cellops.CellStatus`](../../internal/cellops/provisioner.go)
payload. `provision_cell.sh` polls
`GET /api/tenants/{id}/dedicated-cells/{cell_id}` until the cell
flips to `active`.

## Step-by-step

### 1. Validate

`provision_cell.sh` enforces:

- `tenant_id` exists in the tenant store and has
  `contract_type ∈ {b2b_dedicated, sovereign}`.
- `region` matches a known DC region.
- `country` is a valid ISO-3166 alpha-2 code.
- `capacity_petabytes` is between 0.1 and 5.
- `ec_profile` is one of `6+2`, `8+3`, `10+4`, `12+4`, `16+4`.

### 2. Allocate hardware

```bash
cd deploy/local-dc/terraform
terraform workspace new "${cell_id}"
terraform apply -var-file="cells/${cell_id}.tfvars"
```

Production deploys keep one Terraform workspace per cell so
their state files never overlap.

### 3. Deploy Ceph

After hardware is up, run the playbook:

```bash
cd deploy/local-dc/ansible
ansible-playbook -i "inventories/${cell_id}.ini" playbook.yml
```

Then bootstrap the cluster on the first mon node:

```bash
ssh mon-01 "sudo /opt/zkof/cephadm/install.sh \
  --cluster-name zkof-cell-${cell_id} \
  --rgw-realm zkof --rgw-zonegroup zkof --rgw-zone ${cell_id}"
```

### 4. Register with the console

```bash
./register_cell.sh \
  --console-url https://console.zkof.example \
  --admin-token "$ZKOF_ADMIN_TOKEN" \
  --tenant tenant-acme-co \
  --spec ./cell.spec.json
# → returns the cell_id; export ZKOF_CELL_ID=...
```

### 5. Apply placement policy

```bash
./apply_placement.sh \
  --console-url https://console.zkof.example \
  --admin-token "$ZKOF_ADMIN_TOKEN" \
  --tenant tenant-acme-co \
  --provider ceph_rgw \
  --country US \
  --ec-profile 6+2
```

The placement-policy DSL routes by provider name, region, and
country — it has no first-class `cell_id` selector. To pin a
tenant to a specific dedicated cell, register the cell's RGW
endpoint under a unique provider name (via `register_cell.sh` in
the previous step) and pass that name as `--provider` here.

### 6. Hot-load the provider

The gateway picks up new providers on the next config reload.
On the gateway fleet:

```bash
sudo systemctl reload zk-gateway     # SIGHUP — re-reads config.json
```

The fleet must be reloaded one node at a time so quorum is
maintained.

### 7. Flip to active

```bash
./flip_active.sh \
  --console-url https://console.zkof.example \
  --admin-token "$ZKOF_ADMIN_TOKEN" \
  --cell "$ZKOF_CELL_ID"
```

This calls the (Phase 4) `PATCH /api/tenants/{id}/dedicated-cells/{cell_id}`
endpoint with `{"status":"active"}`. Phase 3 ships the
provisioning + decommission paths and returns the cell as
`provisioning`; the flip endpoint is a future addition tracked
on the Phase 4 checklist.

## Decommission

```bash
./provision_cell.sh --decommission --cell "$ZKOF_CELL_ID"
```

Decommission is the reverse: drain via the migration state
machine (`local_only → local_primary_wasabi_drain → ...`),
unregister the cell, then destroy the Terraform workspace.
