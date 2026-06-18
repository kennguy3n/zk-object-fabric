# Wasabi Cost Optimization Runbook

Wasabi is the fabric's low-cost primary storage tier (~$6.99/TB-month
with a fair-use egress policy, see [`docs/PROPOSAL.md`](../PROPOSAL.md)
§2.1). Its headline price comes with one cost trap that is invisible
in the S3 API but very real on the invoice: a **90-day minimum storage
duration**. This runbook explains the minimum, how to configure
lifecycle rules so you don't pay for early deletes, and when to route
ephemeral objects to the cache tier instead.

## 1. The 90-day minimum storage duration

Every object written to Wasabi is billed for **at least 90 days of
storage**, even if you delete it sooner. Delete an object after 10
days and you are still charged for the remaining 80 days of storage on
those bytes. The constant lives in code as
`providers/wasabi.WasabiMinStorageDays` (90) and drives the billing
pipeline's `MinStorageTracker` and the placement engine's cost model
(`providers.ProviderCostModel.MinStorageDurationDay`).

### Estimating the early-delete charge

The residual charge for deleting `sizeBytes` of age `age` is computed
from the **precise** remaining duration of the window (not a rounded
day count), so partial days are billed proportionally:

```
billable_remainder = max(0, 90d - age)        # a duration, sub-day precise
residual_cost_usd  = (sizeBytes / 1e12) * 6.99 * (billable_remainder / 30d)
```

This matches `MinStorageWarning.EstimatedEarlyDeleteCostUSD(sizeBytes)`
exactly. Note that `RemainingDays` (the value shown in the
`X-Zkof-Wasabi-Min-Storage-Remaining-Days` header, see §4) is the same
remainder rounded **up** to whole days for display — so for an object
with, say, 12 hours left, the header reads `1` day while the dollar
estimate prices only the actual ½ day. Use `BillableRemainder` /
`EstimatedEarlyDeleteCostUSD` for billing math and `RemainingDays`
purely as an operator-facing display value.

`providers/wasabi.MinStorageDurationWarning(storedAt, now)` returns
this window state (`WithinMinStorageWindow`, `RemainingDays`,
`BillableRemainder`), and `MinStorageWarning.EstimatedEarlyDeleteCostUSD(sizeBytes)`
turns it into a dollar figure. The data plane surfaces the same signal
operationally — see §4.

## 2. Avoiding early-delete charges with lifecycle rules

The goal is simple: **don't write objects to Wasabi that you intend to
delete within 90 days.** For objects whose lifetime you *can* predict,
configure bucket lifecycle rules so expirations land on or after the
90-day boundary.

Set a lifecycle expiration at **≥ 90 days** so objects are only removed
once they are out of the minimum-storage window:

```bash
aws --endpoint-url "$WASABI_ENDPOINT" s3api put-bucket-lifecycle-configuration \
  --bucket "$BUCKET" \
  --lifecycle-configuration '{
    "Rules": [
      {
        "ID": "expire-after-min-storage-window",
        "Status": "Enabled",
        "Filter": { "Prefix": "" },
        "Expiration": { "Days": 90 }
      }
    ]
  }'
```

Guidance:

- **Never** set `Expiration.Days` below 90 on a Wasabi bucket — every
  expiration before day 90 incurs the residual charge in §1.
- For data you keep indefinitely, no lifecycle rule is needed; the
  minimum only bites on deletes.
- For versioned buckets, remember that overwriting an object creates a
  noncurrent version that is *also* subject to the 90-day minimum.
  Use `NoncurrentVersionExpiration` with `NoncurrentDays >= 90`.
- The fabric exposes bucket lifecycle configuration through its
  S3-compatible API (`PUT /{bucket}?lifecycle`); the same XML/JSON
  applies.

## 3. Use the cache tier for ephemeral objects

Objects with a lifetime shorter than 90 days do **not** belong on
Wasabi. Route them to the Linode-hosted hot object cache
(`cache/hot_object_cache`) instead, which has no minimum-duration
penalty:

- Short-TTL scratch data, intermediate pipeline artifacts, and
  preview/thumbnail derivatives should be written to the cache tier.
- Keep Wasabi for the durable, long-lived copy of record.
- The placement engine reads each provider's cost model, including
  `MinStorageDurationDay`, when deciding where to place an object; keep
  placement policies for short-lived workloads pointed at the cache
  tier rather than Wasabi.

## 4. Operator signal: early-delete warning on DELETE

To make the trap visible at the moment it is triggered, the
S3-compatible DELETE path emits **informational response headers** when
a deleted object lived on Wasabi and was still inside the 90-day
window:

| Header | Meaning |
| --- | --- |
| `X-Zkof-Wasabi-Early-Delete-Warning: true` | The deleted object was within Wasabi's 90-day minimum window. |
| `X-Zkof-Wasabi-Min-Storage-Remaining-Days: N` | Whole days of minimum-storage charge that remain on those bytes. |

These headers are **advisory only** — the DELETE still succeeds (HTTP
`204 No Content`). They are not returned for objects past the 90-day
window, for non-Wasabi backends, or for versioning-enabled deletes that
merely insert a delete marker (no bytes are removed, so no early-delete
charge applies).

**Caveat — objects with no creation timestamp:** the warning is
derived from the object manifest's `CreatedAt`. Manifests written
before creation-timestamp tracking existed carry a zero
`CreatedAt`; for those the gateway cannot determine the object's age,
so it **fails open** and emits no warning, even if the object is in
fact within the 90-day window. This is deliberate (better to stay
silent than assert a charge we can't substantiate), but it means the
header is not a substitute for the billing pipeline's
`MinStorageTracker`, which tracks per-piece age authoritatively. For
older objects, rely on the billing pipeline rather than the DELETE
header.

Operators and console UIs can use these headers (and the
`wasabi_min_storage_remaining_days`-style fields surfaced by listing
APIs as they gain Wasabi awareness) to confirm a delete was
intentional before eating the residual charge.

## 5. Checklist

- [ ] Bucket lifecycle expirations are all `>= 90` days (current and
      noncurrent versions).
- [ ] Short-lived / ephemeral workloads are routed to the cache tier,
      not Wasabi.
- [ ] Placement policies for ephemeral data do not name a Wasabi
      backend.
- [ ] Operators are aware of the `X-Zkof-Wasabi-Early-Delete-Warning`
      response header and treat a non-empty
      `X-Zkof-Wasabi-Min-Storage-Remaining-Days` as "this delete costs
      money."
