# Runbook: Internal mTLS (gateway → Postgres / billing)

This runbook explains how to enable mutual TLS (client authentication)
on the gateway's **outbound** connections to its internal backends:

- the control-plane **Postgres** metadata database, and
- the **ClickHouse** billing sink (HTTP interface).

It closes the audit gap "no mTLS between gateway and Postgres /
billing" (`docs/security/audit-package-security.md` §8). With it
enabled, a compromised internal network can neither read metadata in
transit nor impersonate a backend, because both ends present
certificates.

> This is separate from the per-listener `tls` blocks
> (`gateway.tls`, `console.tls`, `health.tls`), which configure the
> gateway's **inbound** HTTPS listeners and are server-auth only.
> Internal mTLS is the mirror image: the gateway is the *client*.

---

## 1. Configuration

Add a top-level `internal_tls` section and arm it per connection:

```json
{
  "internal_tls": {
    "enabled": true,
    "cert_file": "/etc/zkof/tls/gateway-client.pem",
    "key_file":  "/etc/zkof/tls/gateway-client-key.pem",
    "ca_file":   "/etc/zkof/tls/internal-ca.pem"
  },
  "control_plane": {
    "metadata_dsn": "postgres://gateway@pg.internal:5432/zkof",
    "metadata_tls": true
  },
  "billing": {
    "clickhouse_url": "https://clickhouse.internal:8443",
    "clickhouse_database": "billing",
    "clickhouse_tls": true
  }
}
```

| Field | Meaning |
|-------|---------|
| `internal_tls.enabled` | Master switch. When `false`, the per-connection toggles are ignored and behaviour is unchanged (backwards compatible). |
| `internal_tls.cert_file` / `key_file` | PEM client certificate + private key the gateway presents to backends. |
| `internal_tls.ca_file` | PEM CA bundle used to verify backend server certificates. Required — leaving it empty would fall back to the host's system roots and defeat CA pinning. |
| `control_plane.metadata_tls` | Apply internal mTLS to the Postgres metadata connection. |
| `billing.clickhouse_tls` | Apply internal mTLS to the ClickHouse billing connection. ClickHouse endpoint must be `https://`. |

All three file paths are required when `internal_tls.enabled` is
`true`; the gateway refuses to start otherwise (`config.Validate`),
and fails closed (does not silently downgrade to an unauthenticated
connection) if the cert/key/CA cannot be loaded at boot.

### How each backend applies the material

- **ClickHouse** dials over `net/http`, so the gateway builds a real
  `*tls.Config` (client cert + pinned `RootCAs`, `MinVersion` TLS 1.2)
  and sets it on the sink's HTTP transport.
- **Postgres** uses the registered `lib/pq` driver, which loads TLS
  material **from the connection string**, not from a `*tls.Config`.
  The gateway therefore injects the standard libpq parameters into
  the DSN: `sslcert`, `sslkey`, `sslrootcert`, and — when the DSN does
  not already set one — `sslmode=verify-full`. Any `sslmode` or cert
  path you set explicitly in the DSN is preserved; if you leave a
  non-verifying `sslmode` (e.g. `require`) the gateway logs a warning
  because the backend CA will not actually be checked. Both URL-form
  (`postgres://…`) and keyword-form (`host=… user=…`) DSNs are
  supported.

---

## 2. Generating certificates

You need a CA, plus a client keypair for the gateway. Backends
(Postgres, ClickHouse) each need a server keypair issued by the same
CA. Two common approaches:

### Option A — cfssl

```bash
# 1. CA
cat > ca-csr.json <<'EOF'
{ "CN": "zkof-internal-ca", "key": { "algo": "ecdsa", "size": 256 } }
EOF
cfssl gencert -initca ca-csr.json | cfssljson -bare internal-ca

# 2. Gateway client cert (clientAuth)
cat > client-csr.json <<'EOF'
{ "CN": "zkof-gateway", "key": { "algo": "ecdsa", "size": 256 } }
EOF
cat > profiles.json <<'EOF'
{ "signing": { "profiles": {
  "client": { "expiry": "8760h", "usages": ["signing","digital signature","client auth"] },
  "server": { "expiry": "8760h", "usages": ["signing","digital signature","server auth"] }
}}}
EOF
cfssl gencert -ca internal-ca.pem -ca-key internal-ca-key.pem \
  -config profiles.json -profile client client-csr.json | cfssljson -bare gateway-client

# 3. Backend server cert (serverAuth), hostname must match what the gateway dials
cfssl gencert -ca internal-ca.pem -ca-key internal-ca-key.pem \
  -config profiles.json -profile server \
  -hostname pg.internal,clickhouse.internal server-csr.json | cfssljson -bare backend-server
```

Map to config: `cert_file=gateway-client.pem`,
`key_file=gateway-client-key.pem`, `ca_file=internal-ca.pem`.

### Option B — openssl

```bash
# 1. CA
openssl ecparam -name prime256v1 -genkey -noout -out internal-ca-key.pem
openssl req -x509 -new -key internal-ca-key.pem -days 3650 \
  -subj "/CN=zkof-internal-ca" -out internal-ca.pem

# 2. Gateway client cert
openssl ecparam -name prime256v1 -genkey -noout -out gateway-client-key.pem
openssl req -new -key gateway-client-key.pem -subj "/CN=zkof-gateway" -out client.csr
openssl x509 -req -in client.csr -CA internal-ca.pem -CAkey internal-ca-key.pem \
  -CAcreateserial -days 365 \
  -extfile <(printf "extendedKeyUsage=clientAuth") -out gateway-client.pem

# 3. Backend server cert (SAN must match the dialed host)
openssl ecparam -name prime256v1 -genkey -noout -out backend-server-key.pem
openssl req -new -key backend-server-key.pem -subj "/CN=pg.internal" -out server.csr
openssl x509 -req -in server.csr -CA internal-ca.pem -CAkey internal-ca-key.pem \
  -CAcreateserial -days 365 \
  -extfile <(printf "subjectAltName=DNS:pg.internal,DNS:clickhouse.internal\nextendedKeyUsage=serverAuth") \
  -out backend-server.pem
```

> `sslmode=verify-full` validates the server **hostname** against the
> certificate SAN, so the SAN must include the host in `metadata_dsn`.
> If you only want chain validation without hostname checking, set
> `sslmode=verify-ca` explicitly in the DSN.

File permissions: private keys should be `0600` and owned by the
gateway's service user.

---

## 3. Backend prerequisites

- **Postgres**: configure the server with `ssl = on`, a server cert
  issued by the internal CA, and (to require client certs)
  `clientcert=verify-full` in `pg_hba.conf` for the gateway role.
- **ClickHouse**: enable the HTTPS port (e.g. `8443`), point it at a
  server cert from the internal CA, and set
  `<verificationMode>strict</verificationMode>` with the CA in
  `caConfig` to require client certs.

---

## 4. Rollout

1. Generate and distribute the CA + gateway client keypair to the
   gateway hosts; issue server certs to Postgres / ClickHouse.
2. Stand up the backends with TLS enabled but **not yet requiring**
   client certs.
3. Set `internal_tls.enabled = true` and the relevant per-connection
   toggle(s). Roll one backend at a time (`metadata_tls` first, then
   `clickhouse_tls`) so you can isolate any failure.
4. Watch the gateway logs at boot:
   - `internal mTLS enabled for metadata Postgres connection (...)`
   - `internal mTLS enabled for clickhouse billing connection (...)`
   - any `WARNING` about a non-verifying `sslmode` or a non-`https`
     ClickHouse URL.
5. Once connections are healthy, flip the backends to **require**
   client certs.

## 5. Rollback

Set `internal_tls.enabled = false` (or the specific per-connection
toggle) and restart the gateway. The connection reverts to its prior
behaviour: the DSN's own `sslmode` for Postgres and the default
transport for ClickHouse. No config fields need to be removed.

## 6. Troubleshooting

| Symptom | Likely cause |
|---------|--------------|
| Gateway exits at boot: `internal_tls.enabled is true but ... must be set` | One of `cert_file`/`key_file`/`ca_file` is empty. |
| Gateway exits: `internal_tls load client keypair ...` | Cert/key paths wrong, unreadable, or key does not match cert. |
| Gateway exits: `ca_file ... contains no PEM certificates` | `ca_file` is not a PEM bundle. |
| Postgres: `x509: certificate signed by unknown authority` | Backend server cert not issued by `ca_file`'s CA. |
| Postgres: `x509: certificate is valid for X, not Y` | DSN host not in the server cert SAN (with `verify-full`). |
| Log warns `non-verifying sslmode` | DSN sets `sslmode=disable/allow/prefer/require`; raise to `verify-full`. |
| ClickHouse: client cert not presented | `clickhouse_url` is not `https://` (see boot warning). |
