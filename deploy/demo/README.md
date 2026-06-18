# Local Ceph RGW (S3) demo

A single-command, single-node [Ceph RADOS Gateway](https://docs.ceph.com/en/latest/radosgw/)
that exposes an S3-compatible endpoint on `:8888`. It backs two
documented workflows without a real Ceph cluster:

- **Load-testing Tier 2** — `docs/runbooks/load-testing.md` §3 runs the
  benchmark harness against `-provider=ceph_rgw -rgw-endpoint=http://localhost:8888`.
- **Live conformance gate** — `tests/s3_compat` `TestSuite_CephRGW`
  (see `docs/runbooks/s3-conformance.md` and `deploy/local-dc/README.md`).

The boot parameters mirror the `Start Ceph RGW demo container` step in
`.github/workflows/ci.yml`, so a local run reproduces the CI conformance
gate exactly.

> **Development only.** All-in-one mon/mgr/osd/rgw in one container, a
> single OSD, no replication, plain HTTP. Do not use for anything real.

## Quick start

```bash
docker compose -f deploy/demo/docker-compose.yml up ceph-rgw
# (or: cd deploy/demo && docker compose up ceph-rgw)
```

The gateway is ready when the container reports `healthy`
(`docker ps`). First boot takes ~30–60s while Ceph initializes.

| Setting     | Value                                            |
| ----------- | ------------------------------------------------ |
| Endpoint    | `http://localhost:8888`                          |
| Access key  | `zkof-test-ak`                                   |
| Secret key  | `zkof-test-sk-0000000000000000000000000`         |
| Region      | `us-east-1` (RGW demo accepts any; `default` ok) |
| Bucket      | `bench` (auto-created at boot)                   |

## Try it with the AWS CLI

```bash
export AWS_ACCESS_KEY_ID=zkof-test-ak
export AWS_SECRET_ACCESS_KEY=zkof-test-sk-0000000000000000000000000
export AWS_DEFAULT_REGION=us-east-1

echo hello > hello.txt
aws --endpoint-url http://localhost:8888 s3 cp hello.txt s3://bench/
aws --endpoint-url http://localhost:8888 s3 ls s3://bench/
```

## Run the conformance suite

`TestSuite_CephRGW` is skipped unless `CEPH_RGW_ENDPOINT` is set. Create
its bucket once, then point the suite at the demo:

```bash
AWS_ACCESS_KEY_ID=zkof-test-ak \
AWS_SECRET_ACCESS_KEY=zkof-test-sk-0000000000000000000000000 \
AWS_DEFAULT_REGION=us-east-1 \
  aws --endpoint-url http://localhost:8888 \
      s3api create-bucket --bucket zkof-ceph-compliance

CEPH_RGW_ENDPOINT=http://localhost:8888 \
CEPH_RGW_BUCKET=zkof-ceph-compliance \
CEPH_RGW_ACCESS_KEY=zkof-test-ak \
CEPH_RGW_SECRET_KEY=zkof-test-sk-0000000000000000000000000 \
CEPH_RGW_REGION=us-east-1 \
  go test -v -run TestSuite_CephRGW ./tests/s3_compat/ -timeout 15m
```

## Teardown

```bash
docker compose -f deploy/demo/docker-compose.yml down -v
```

`-v` drops the `ceph-etc` / `ceph-lib` volumes so the next `up` starts
from a clean cluster.
