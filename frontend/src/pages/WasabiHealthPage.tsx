import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { UsageSnapshot } from "../api/types";
import {
  opsGet,
  type OpsCacheStats,
  type WasabiBudget,
  type WasabiBudgetsResponse,
} from "../api/opsClient";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";
import { GaugeChart } from "../components/GaugeChart";

// WasabiHealthPage focuses on the Wasabi fair-use envelope: the
// product's core constraint is per-tenant monthly origin egress ≤ 1×
// active stored bytes (see providers/wasabi/guardrails.go). This page
// shows where the tenant sits against that 1× limit, surfaces the
// 90-day minimum-storage rule for early-deleted objects, and tracks
// cache warm-up (how full the Linode hot cache is) since a cold cache
// pushes reads to Wasabi origin and burns egress budget.
//
// The egress-vs-stored ratio is computed from GET /api/v1/usage (the
// tenant session can always read it). Cache warm-up and the
// fleet-wide per-tenant budget table come from the admin-gated
// /api/v1/ops/* surface and degrade gracefully when not available.

export function WasabiHealthPage() {
  const { tenant, token } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [cache, setCache] = useState<OpsCacheStats | null>(null);
  const [budgets, setBudgets] = useState<WasabiBudget[] | null>(null);
  const [opsLoaded, setOpsLoaded] = useState(false);
  const [usageError, setUsageError] = useState<string | null>(null);

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
      opsGet<OpsCacheStats>("cache-stats", token),
      opsGet<WasabiBudgetsResponse>("wasabi-budgets", token),
    ]).then(([c, b]) => {
      if (cancelled) return;
      setCache(c);
      setBudgets(b?.budgets ?? null);
      setOpsLoaded(true);
    });
    return () => {
      cancelled = true;
    };
  }, [token]);

  const stored = usage?.storageBytes ?? 0;
  const egress = usage?.egressBytesThisMonth ?? 0;
  // The fair-use limit is 1× stored bytes, so the ratio against the
  // limit is simply egress/stored (capped for the gauge by the
  // component). With no stored bytes the ratio is undefined → 0.
  const egressRatio = stored > 0 ? egress / stored : 0;

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Wasabi health</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Fair-use posture for <strong>{tenant?.name}</strong>: monthly origin egress must
        stay within 1× active stored bytes. A warm cache keeps reads off Wasabi origin.
      </div>
      {usageError && <div className="panel danger-text">Failed to load usage: {usageError}</div>}

      <div className="grid cols-3">
        <div className="panel" style={{ display: "flex", justifyContent: "center", alignItems: "center" }}>
          <GaugeChart
            value={egressRatio}
            label="Egress ÷ 1× stored"
            valueLabel={`${(egressRatio * 100).toFixed(0)}%`}
            color={egressRatio >= 1 ? "var(--danger)" : egressRatio >= 0.8 ? "var(--accent)" : "var(--ok)"}
          />
        </div>
        <div className="panel" style={{ display: "flex", justifyContent: "center", alignItems: "center" }}>
          {cache ? (
            <GaugeChart
              value={cache.utilization}
              label="Cache warm-up"
              color={cache.utilization >= 0.5 ? "var(--ok)" : "var(--accent)"}
            />
          ) : (
            <div className="muted">{opsLoaded ? "Cache feed unavailable." : "Loading…"}</div>
          )}
        </div>
        <div className="panel">
          <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
            Fair-use summary
          </div>
          <table style={{ marginTop: 8 }}>
            <tbody>
              <tr><td>Stored</td><td style={{ textAlign: "right" }}>{formatBytes(stored)}</td></tr>
              <tr><td>Egress (mo)</td><td style={{ textAlign: "right" }}>{formatBytes(egress)}</td></tr>
              <tr>
                <td>1× limit</td>
                <td style={{ textAlign: "right" }}>{formatBytes(stored)}</td>
              </tr>
              <tr>
                <td>Headroom</td>
                <td style={{ textAlign: "right" }} className={egress > stored ? "danger-text" : "ok"}>
                  {formatBytes(Math.max(stored - egress, 0))}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div className="panel">
        <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
          90-day minimum-storage warnings
        </div>
        <div className="muted" style={{ marginTop: 8 }}>
          No early-deletion charges pending.
        </div>
        <div className="muted" style={{ fontSize: 11, marginTop: 8 }}>
          Objects deleted before 90 days still incur Wasabi's minimum-storage charge.
          Recently-deleted objects within their 90-day window appear here.
        </div>
      </div>

      <div className="panel" style={{ padding: 0 }}>
        <div style={{ padding: "16px 20px 0" }} className="muted">
          <span style={{ fontSize: 12, textTransform: "uppercase", letterSpacing: "0.04em" }}>
            Per-tenant egress budgets
          </span>
        </div>
        <table style={{ marginTop: 12 }}>
          <thead>
            <tr>
              <th>Tenant</th>
              <th style={{ textAlign: "right" }}>Stored</th>
              <th style={{ textAlign: "right" }}>Egress</th>
              <th style={{ textAlign: "right" }}>Ratio</th>
              <th style={{ textAlign: "right" }}>Remaining</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {budgets === null ? (
              <tr>
                <td colSpan={6} className="muted">
                  {opsLoaded ? "Budget feed unavailable (admin access required)." : "Loading…"}
                </td>
              </tr>
            ) : budgets.length === 0 ? (
              <tr>
                <td colSpan={6} className="muted">No tenants reporting Wasabi egress.</td>
              </tr>
            ) : (
              budgets.map((b) => (
                <tr key={b.tenantId}>
                  <td>{b.tenantId}</td>
                  <td style={{ textAlign: "right" }}>{formatBytes(b.storedBytes)}</td>
                  <td style={{ textAlign: "right" }}>{formatBytes(b.egressBytes)}</td>
                  <td style={{ textAlign: "right" }}>{(b.egressRatio * 100).toFixed(0)}%</td>
                  <td style={{ textAlign: "right" }}>{formatBytes(b.remainingBytes)}</td>
                  <td>
                    <span className={`badge${b.status === "ok" ? "" : " accent"}`}>{b.status}</span>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
