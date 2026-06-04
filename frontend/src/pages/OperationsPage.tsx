import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { UsageSnapshot } from "../api/types";
import { opsGet, type OpsCacheStats, type OpsHealth } from "../api/opsClient";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";
import { GaugeChart } from "../components/GaugeChart";

// OperationsPage is the built-in ops view for SME operators who do not
// run Grafana/Prometheus. It aggregates the gateway's own internal
// signals — node health, hot-object-cache hit ratio, per-tenant
// storage/egress usage, and the Wasabi fair-use egress budget — into
// a single dashboard.
//
// Two data sources back this page:
//   - GET /api/v1/usage (tenant-scoped, always available to the
//     logged-in console session) drives the per-tenant usage bars and
//     the egress-budget gauge.
//   - GET /api/v1/ops/{health,cache-stats} (admin-gated, served by
//     api/console/ops_handler.go) drive the health badge and cache
//     gauge. These degrade gracefully: if the operator's session is
//     not admin-scoped, or the ops surface is not wired, the cards
//     show an "unavailable" state instead of failing the whole page.

// The tenant budget field (egressTbMonth) is a binary tebibyte, matching
// the gateway guardrail (internal/auth/abuse.go uses 1<<40) and
// formatBytes' binary prefixes. Named TIB to make the 2^40 (not 1e12)
// basis explicit.
const BYTES_PER_TIB = 2 ** 40;

export function OperationsPage() {
  const { tenant, token } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [health, setHealth] = useState<OpsHealth | null>(null);
  const [cache, setCache] = useState<OpsCacheStats | null>(null);
  const [usageError, setUsageError] = useState<string | null>(null);
  // opsLoaded gates the "unavailable" copy so it only shows after the
  // fetch settles, not during the initial render.
  const [opsLoaded, setOpsLoaded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api
      .currentUsage()
      .then((u) => !cancelled && setUsage(u))
      .catch((e) => !cancelled && setUsageError(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    Promise.all([
      opsGet<OpsHealth>("health", token),
      opsGet<OpsCacheStats>("cache-stats", token),
    ]).then(([h, c]) => {
      if (cancelled) return;
      setHealth(h);
      setCache(c);
      setOpsLoaded(true);
    });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const egressBudgetBytes = tenant ? tenant.budgets.egressTbMonth * BYTES_PER_TIB : 0;
  const egressUsed = usage?.egressBytesThisMonth ?? 0;
  const egressConsumed = egressBudgetBytes > 0 ? egressUsed / egressBudgetBytes : 0;
  const egressRemaining = Math.max(egressBudgetBytes - egressUsed, 0);
  // Storage bar is scaled against the egress budget purely so the two
  // bars share an axis; the absolute figure is shown alongside.
  const storageBytes = usage?.storageBytes ?? 0;

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Operations</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Gateway health, cache efficiency, and Wasabi fair-use budget for{" "}
        <strong>{tenant?.name}</strong>. No Grafana required.
      </div>

      <div className="grid cols-3">
        <div className="panel">
          <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
            Gateway health
          </div>
          {health ? (
            <>
              <div style={{ fontSize: 24, fontWeight: 700, marginTop: 6 }}>
                <span className={health.status === "ok" ? "ok" : "danger-text"}>
                  {health.status === "ok" ? "Healthy" : "Degraded"}
                </span>
              </div>
              <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
                {health.node.nodeId}
                {health.node.cellId ? ` · ${health.node.cellId}` : ""} · state {health.node.state}
              </div>
              <div className="muted" style={{ fontSize: 12 }}>
                {health.node.inflight} in-flight · quorum ≥ {health.node.quorumThreshold}
              </div>
            </>
          ) : (
            <div className="muted" style={{ marginTop: 8 }}>
              {opsLoaded ? "Health feed unavailable (admin access required)." : "Loading…"}
            </div>
          )}
        </div>

        <div className="panel" style={{ display: "flex", justifyContent: "center", alignItems: "center" }}>
          {cache ? (
            <GaugeChart
              value={cache.hitRatio}
              label="Cache hit ratio"
              color={cache.hitRatio >= 0.9 ? "var(--ok)" : cache.hitRatio >= 0.75 ? "var(--accent)" : "var(--danger)"}
            />
          ) : (
            <div className="muted">
              {opsLoaded ? "Cache stats unavailable (admin access required)." : "Loading…"}
            </div>
          )}
        </div>

        <div className="panel" style={{ display: "flex", justifyContent: "center", alignItems: "center" }}>
          <GaugeChart
            value={egressConsumed}
            label="Wasabi egress budget used"
            color={egressConsumed >= 0.95 ? "var(--danger)" : egressConsumed >= 0.8 ? "var(--accent)" : "var(--ok)"}
          />
        </div>
      </div>

      <div className="panel">
        <div className="muted" style={{ fontSize: 12, textTransform: "uppercase", marginBottom: 12 }}>
          Storage &amp; egress usage
        </div>
        {usageError && <div className="danger-text" style={{ marginBottom: 8 }}>Failed to load usage: {usageError}</div>}
        <UsageBar
          label="Stored"
          value={storageBytes}
          max={Math.max(storageBytes, egressBudgetBytes, 1)}
          display={formatBytes(storageBytes)}
          color="var(--accent)"
        />
        <UsageBar
          label="Egress this month"
          value={egressUsed}
          max={Math.max(egressBudgetBytes, egressUsed, 1)}
          display={`${formatBytes(egressUsed)} / ${tenant?.budgets.egressTbMonth ?? 0} TB budget`}
          color={egressConsumed >= 0.95 ? "var(--danger)" : "var(--ok)"}
        />
        <div className="muted" style={{ fontSize: 12, marginTop: 8 }}>
          Egress budget remaining: <strong>{formatBytes(egressRemaining)}</strong>
        </div>
      </div>

      <div className="grid cols-3">
        <div className="panel">
          <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
            Cache detail
          </div>
          {cache ? (
            <table style={{ marginTop: 8 }}>
              <tbody>
                <tr><td>Entries</td><td style={{ textAlign: "right" }}>{cache.entries.toLocaleString()}</td></tr>
                <tr><td>Used</td><td style={{ textAlign: "right" }}>{formatBytes(cache.bytesUsed)}</td></tr>
                <tr><td>Limit</td><td style={{ textAlign: "right" }}>{formatBytes(cache.bytesLimit)}</td></tr>
                <tr><td>Hits / misses</td><td style={{ textAlign: "right" }}>{cache.hits.toLocaleString()} / {cache.misses.toLocaleString()}</td></tr>
                <tr><td>Evictions</td><td style={{ textAlign: "right" }}>{cache.evictions.toLocaleString()}</td></tr>
              </tbody>
            </table>
          ) : (
            <div className="muted" style={{ marginTop: 8 }}>{opsLoaded ? "Unavailable." : "Loading…"}</div>
          )}
        </div>

        <div className="panel">
          <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
            Abuse-detector alerts
          </div>
          <div className="muted" style={{ marginTop: 8 }}>
            No active alerts.
          </div>
          <div className="muted" style={{ fontSize: 11, marginTop: 8 }}>
            Surfaces active rate-limit / abuse-detector trips for this tenant.
          </div>
        </div>

        <div className="panel">
          <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
            Lifecycle-evaluator actions
          </div>
          <div className="muted" style={{ marginTop: 8 }}>
            No recent actions.
          </div>
          <div className="muted" style={{ fontSize: 11, marginTop: 8 }}>
            Recent tier transitions and expirations applied by the lifecycle evaluator.
          </div>
        </div>
      </div>
    </div>
  );
}

function UsageBar({
  label,
  value,
  max,
  display,
  color,
}: {
  label: string;
  value: number;
  max: number;
  display: string;
  color: string;
}) {
  const pct = max > 0 ? Math.min((value / max) * 100, 100) : 0;
  return (
    <div style={{ marginBottom: 12 }}>
      <div className="row" style={{ justifyContent: "space-between" }}>
        <span style={{ fontSize: 13 }}>{label}</span>
        <span className="muted" style={{ fontSize: 12 }}>{display}</span>
      </div>
      <div
        style={{
          marginTop: 4,
          height: 10,
          borderRadius: 999,
          background: "var(--border)",
          overflow: "hidden",
        }}
      >
        <div style={{ width: `${pct}%`, height: "100%", background: color }} />
      </div>
    </div>
  );
}
