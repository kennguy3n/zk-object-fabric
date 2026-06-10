import { describe, expect, it } from "vitest";

import { usageSnapshotFromCounters } from "./client";

const GIB = 2 ** 30;

describe("usageSnapshotFromCounters", () => {
  it("converts storage_bytes_seconds (a byte-seconds integral) to average bytes held", () => {
    // A constant 1 GiB held for the whole window yields a byte-seconds
    // counter of 1 GiB × windowSeconds. The snapshot must recover the
    // ~1 GiB average, NOT the (windowSeconds×-larger) raw counter that
    // the old mapping surfaced verbatim.
    const start = "2026-06-01T00:00:00Z";
    const end = "2026-07-01T00:00:00Z";
    const windowSeconds = (Date.parse(end) - Date.parse(start)) / 1000;
    const snap = usageSnapshotFromCounters({
      tenant_id: "t1",
      start,
      end,
      counters: { storage_bytes_seconds: GIB * windowSeconds },
    });
    expect(snap.storageBytes).toBeCloseTo(GIB, 0);
    // Guard the dimensional intent: the raw counter is windowSeconds×
    // the average, so we must have divided it down, not passed through.
    expect(snap.storageBytes).toBeLessThan(GIB * windowSeconds);
  });

  it("returns 0 stored bytes for a non-positive window instead of dividing by zero", () => {
    const snap = usageSnapshotFromCounters({
      tenant_id: "t1",
      start: "2026-06-01T00:00:00Z",
      end: "2026-06-01T00:00:00Z",
      counters: { storage_bytes_seconds: 123456 },
    });
    expect(snap.storageBytes).toBe(0);
  });

  it("sums request verbs and passes egress + identity through", () => {
    const snap = usageSnapshotFromCounters({
      tenant_id: "t2",
      start: "2026-06-01T00:00:00Z",
      end: "2026-06-08T00:00:00Z",
      counters: {
        get_requests: 10,
        put_requests: 5,
        list_requests: 3,
        delete_requests: 2,
        egress_bytes: 9000,
      },
    });
    expect(snap.requestsLast30Days).toBe(20);
    expect(snap.egressBytesThisMonth).toBe(9000);
    expect(snap.tenantId).toBe("t2");
    expect(snap.monthStart).toBe("2026-06-01T00:00:00Z");
  });

  it("defaults every missing counter to 0", () => {
    const snap = usageSnapshotFromCounters({
      tenant_id: "t3",
      start: "2026-06-01T00:00:00Z",
      end: "2026-07-01T00:00:00Z",
    });
    expect(snap.storageBytes).toBe(0);
    expect(snap.requestsLast30Days).toBe(0);
    expect(snap.egressBytesThisMonth).toBe(0);
  });
});
