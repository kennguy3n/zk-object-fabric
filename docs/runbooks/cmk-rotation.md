# Customer Master Key (CMK) Rotation Runbook

This runbook covers rotation, version pinning, and emergency
revocation of the gateway's Customer Master Key (CMK), which wraps
every per-object Data Encryption Key (DEK) before the manifest
store sees it. Phase 3 supports three CMK backends:

| Scheme              | Wrapper              | Source                                                           |
| ------------------- | -------------------- | ---------------------------------------------------------------- |
| `cmk://local/...`   | `LocalFileWrapper`   | `encryption/client_sdk/local_file_wrapper.go` (dev / tests only) |
| `kms://...` / `arn:aws:kms:...` | `KMSWrapper`         | `encryption/client_sdk/kms_wrapper.go` (AWS KMS, `aws-kms-wrap-v1`) |
| `vault://...` / `transit://...` | `VaultWrapper`       | `encryption/client_sdk/vault_wrapper.go` (HashiCorp Vault transit, `vault-transit-wrap-v1`) |

`cmd/gateway/main.go#buildGatewayEncryption` selects the wrapper at
boot from the `cmk_uri` scheme; the per-manifest wrapped-DEK records
the algorithm tag and key ID used at write time, so old objects
remain readable as long as the previous key version is still
decryptable by the wrapper.

## Operating model

Every wrapper is **decrypt-by-key-version**. A rotation is
**non-destructive**: a new key version is created, new writes are
re-keyed onto it, and old writes continue to decrypt against their
original version. A revocation is **destructive**: the previous key
material is destroyed and any object whose wrapped DEK references
that version becomes permanently unreadable.

Rotate proactively (every 365 days, or on any suspicion of CMK
material exposure). Revoke only when the CMK is known to be
compromised and the trade-off of unreadability is accepted.

---

## 1. AWS KMS rotation (production B2C / B2B managed)

### Pre-flight

- Confirm the gateway is running with `cmk_uri` set to a KMS ARN or
  `kms://...` URL (check `gateway: encryption wrapper=aws-kms-wrap-v1`
  in startup logs).
- Verify the IAM role attached to gateway nodes has
  `kms:GenerateDataKey`, `kms:Decrypt`, and (for rotation operator
  only) `kms:CreateAlias`, `kms:UpdateAlias`, and
  `kms:ScheduleKeyDeletion`.
- Confirm a recent backup of the Postgres manifest store exists
  (manifests carry the wrapped DEKs).

### Procedure: rolling rotation (zero downtime)

1. **Create the new key version.** AWS KMS supports automatic
   yearly rotation on symmetric keys; for manual rotation, create a
   new KMS key and alias it under a fresh alias:

   ```bash
   aws kms create-key \
     --description "zkof-cmk-prod-2026-q2" \
     --key-usage ENCRYPT_DECRYPT \
     --key-spec SYMMETRIC_DEFAULT \
     --tags TagKey=service,TagValue=zk-object-fabric \
            TagKey=tier,TagValue=production
   # → { KeyMetadata: { Arn: arn:aws:kms:us-east-1:123456789012:key/abcd... } }

   aws kms create-alias \
     --alias-name alias/zkof-cmk-prod-current \
     --target-key-id <new-key-arn>
   ```

2. **Grant gateway role decrypt on both keys.** During the rollover
   window the fleet must be able to decrypt manifests written
   against either the old or the new key:

   ```bash
   aws kms put-key-policy --key-id <old-key-arn> --policy-name default \
     --policy file://policies/zkof-gateway-decrypt.json
   aws kms put-key-policy --key-id <new-key-arn> --policy-name default \
     --policy file://policies/zkof-gateway-decrypt.json
   ```

3. **Stage the new ARN onto a canary gateway.** Update the canary
   node's config to set `encryption.cmk_uri` to the new ARN and
   restart it:

   ```bash
   ssh gw-canary-1 "sudo systemctl restart zk-gateway"
   journalctl -u zk-gateway -n 50 | grep "encryption wrapper"
   # → gateway: encryption wrapper=aws-kms-wrap-v1 key=arn:aws:kms:.../<new-key-id>
   ```

   Drive a synthetic PUT/GET workload at the canary; confirm new
   manifests record the new KeyId and old manifests still serve.

4. **Roll the rest of the fleet.** Update each gateway node's
   `encryption.cmk_uri` to the new ARN one node at a time and
   restart. The migration is per-object, lazy, and silent: any new
   PUT after the restart re-keys onto the new KeyId; existing
   objects keep their original wrapped DEK until they are
   rewritten.

5. **Verify.** Pull a sample of recent manifests from the manifest
   store and confirm the `wrapped_dek` algorithm tag is
   `aws-kms-wrap-v1` and the embedded `key_id` matches the new
   ARN.

   ```sql
   SELECT object_key,
          wrapped_dek->>'algorithm' AS alg,
          wrapped_dek->>'key_id'    AS key_id
     FROM manifests
    WHERE created_at > NOW() - INTERVAL '1 hour'
    LIMIT 20;
   ```

6. **Schedule old key deletion (≥ 30 days out).** AWS KMS enforces
   a 7–30 day pending-deletion window; do not schedule deletion
   until you've re-encrypted (or accepted the loss of) every
   manifest still pointing at the old ARN. A safe default is to
   leave the old key live indefinitely and only delete it after
   confirming a full rebalancer pass has rewritten every object.

### Procedure: emergency revocation

Use this only when key material is known to be exposed.

1. **Disable the key.** This blocks all new `Decrypt` calls
   without scheduling deletion, so it's reversible if you change
   your mind:

   ```bash
   aws kms disable-key --key-id <compromised-key-arn>
   ```

   Every gateway PUT/GET against an object whose wrapped DEK
   references this key will now fail with `KMSInvalidStateException`.

2. **Page the on-call.** GETs against affected objects start
   returning 503; alert routing should already be lit up via the
   abuse alert webhook (`config.abuse.alert_webhook_url`) but
   confirm.

3. **Schedule key deletion.** Once the incident response team has
   approved the destructive action:

   ```bash
   aws kms schedule-key-deletion \
     --key-id <compromised-key-arn> --pending-window-in-days 7
   ```

4. **Mark affected objects as lost.** Run the
   `zkof-cmk-revoke` console job (or a manual `UPDATE manifests
   SET state='cmk_revoked'`) so the data plane returns 410 Gone
   instead of a confusing 503.

---

## 2. HashiCorp Vault transit rotation (B2B sovereign / on-prem)

Vault transit is the recommended CMK for B2B dedicated cells
where customers want to hold their own KEKs. Rotation uses
Vault's built-in `rotate` endpoint, which appends a new version
to the same named key and lets all prior versions remain
decryptable.

### Pre-flight

- Confirm `cmk_uri=vault://<addr>/<mount>/<key>` (or
  `transit://...`) in gateway config. The wrapper resolves the
  Vault address via `cfg.Encryption.VaultAddr` (env fallback
  `VAULT_ADDR`) and the auth token via
  `cfg.Encryption.VaultToken` (env fallback `VAULT_TOKEN`).
- Verify Vault is unsealed and reachable from every gateway:
  `curl -sf $VAULT_ADDR/v1/sys/health`.
- The gateway's Vault token must carry an ACL policy allowing
  `update` on `<mount>/encrypt/<key>` and `<mount>/decrypt/<key>`.

### Procedure: rolling rotation

1. **Rotate the transit key.** This appends version *N+1* to the
   same key:

   ```bash
   vault write -f transit/keys/zkof-cmk-prod/rotate
   vault read transit/keys/zkof-cmk-prod
   # → latest_version: 5, min_decryption_version: 1
   ```

2. **Pin the minimum decryption version (optional).** If you
   want to phase out an old version once all objects have been
   re-keyed:

   ```bash
   vault write transit/keys/zkof-cmk-prod/config \
     min_decryption_version=4
   ```

3. **Verify gateway pickup.** No restart is needed: `VaultWrapper`
   re-resolves the latest version on every `Encrypt`. Confirm new
   manifests record the new `key_version`:

   ```sql
   SELECT wrapped_dek->>'key_version' AS v, COUNT(*)
     FROM manifests
    WHERE created_at > NOW() - INTERVAL '15 minutes'
    GROUP BY 1;
   ```

### Procedure: emergency revocation

```bash
# Step 1: disable the transit key (stops new Encrypt and Decrypt calls).
vault write transit/keys/zkof-cmk-prod/config deletion_allowed=true
vault delete transit/keys/zkof-cmk-prod
```

After deletion, any object whose wrapped DEK references that key
becomes permanently unreadable. Same incident response steps as
KMS revocation apply (page on-call, mark affected manifests
`cmk_revoked`).

---

## 3. Local file wrapper (dev only)

`LocalFileWrapper` is for tests and the docker-compose dev stack;
do not use it in production. To "rotate" in dev:

```bash
openssl rand -out /var/lib/zkof/cmk-v2.key 32
# Update cmk_uri to cmk://local/var/lib/zkof/cmk-v2.key
sudo systemctl restart zk-gateway
```

Old objects written under cmk-v1 will be unreadable; re-run the
seed script to repopulate.

---

## 4. Verification matrix

After any rotation or revocation, run the post-change validation
suite:

| Check                                       | Command                                                                                                               | Expected                                  |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- | ----------------------------------------- |
| Gateway boots with new CMK                  | `journalctl -u zk-gateway -n 50 \| grep encryption`                                                                   | `wrapper=...` line names new key          |
| New PUT records new key version             | `aws s3 cp /tmp/probe s3://zkof-canary/probe-$(date +%s)` then inspect manifest                                       | `key_id` / `key_version` matches new key  |
| Old manifests still GET                     | `aws s3 cp s3://zkof-canary/probe-prev /tmp/out`                                                                      | bytes match originally written            |
| Compliance suite passes against new key     | `go test -v -run TestSuite_CephRGW ./tests/s3_compat/`                                                                | 13/13 subtests PASS                       |
| Abuse alerter sees no new errors            | watch `internal/auth/webhook_alert_sink.go` POST destination for 5 minutes                                            | no `cmk_decrypt_failed` events            |

---

## 5. Audit log expectations

Every wrapper logs a structured audit line on rotation events
(KMS: `kms_key_change`, Vault: `vault_key_rotate`). The console
admin token (`cfg.Console.AdminToken`) is required to query the
rotation log via `GET /api/admin/cmk/audit`.

---

## 6. Related code

- `cmd/gateway/main.go#buildGatewayEncryption` — selects wrapper from `cmk_uri`.
- `encryption/client_sdk/kms_wrapper.go` — AWS KMS implementation.
- `encryption/client_sdk/vault_wrapper.go` — Vault transit implementation.
- `encryption/client_sdk/local_file_wrapper.go` — dev-only fallback.
- `internal/config/config.go#EncryptionConfig` — `KMSRegion`, `VaultAddr`, `VaultToken`, `VaultTransitMount`.

---

## 7. Open follow-ups for Phase 4

- Per-tenant CMK so a tenant's key compromise never leaks
  another tenant's data. Tracked in `docs/PROPOSAL.md` Phase 4.
- Automated re-keying job that walks the manifest store and
  rewrites wrapped DEKs onto the latest key version on a schedule,
  so old key versions can be retired without waiting for organic
  rewrites.
