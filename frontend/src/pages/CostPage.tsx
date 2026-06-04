import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { TierConfig, UsageSnapshot } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";

// CostPage gives SME operators a unified, plain-language estimate of
// their monthly bill. The product packages Wasabi storage, Linode
// compute, and the AWS control plane into a single per-TB tier price
// (metadata/tenant.TierConfig.price_per_tb_month), so the headline
// number is the tenant's stored volume × that all-in rate. The page
// pulls live usage from GET /api/v1/usage and the tier price book from
// GET /api/v1/tiers — both reachable from the tenant console session,
// so this view needs no admin scope.

// Binary tebibyte (2^40), matching the gateway guardrail
// (internal/auth/abuse.go uses 1<<40) and formatBytes' binary prefixes.
// Named TIB to make the 2^40 (not 1e12) basis explicit.
const BYTES_PER_TIB = 2 ** 40;

export function CostPage() {
  const { tenant } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [tiers, setTiers] = useState<TierConfig[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    Promise.all([api.currentUsage(), api.listTierConfigs()])
      .then(([u, t]) => {
        if (cancelled) return;
        setUsage(u);
        setTiers(t);
      })
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, []);

  const tier = tiers.find((t) => t.tier === tenant?.licenseTier);
  const storedTB = (usage?.storageBytes ?? 0) / BYTES_PER_TIB;
  const egressTB = (usage?.egressBytesThisMonth ?? 0) / BYTES_PER_TIB;
  const pricePerTB = tier?.price_per_tb_month ?? 0;
  const storageCost = storedTB * pricePerTB;

  // Wasabi fair-use bundles egress up to 1× stored volume into the
  // per-TB price, so in-budget egress is $0 incremental. We surface
  // the in-budget split for transparency rather than billing it twice.
  const egressBudgetTB = tenant?.budgets.egressTbMonth ?? 0;
  const egressOverage = Math.max(egressTB - egressBudgetTB, 0);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Cost</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Estimated monthly cost for <strong>{tenant?.name}</strong>, based on live
        usage and the <strong>{tier?.display_name ?? tenant?.licenseTier}</strong> tier
        price book. The per-TB rate bundles Wasabi storage, Linode compute, and the
        AWS control plane.
      </div>
      {error && <div className="panel danger-text">Failed to load cost inputs: {error}</div>}

      <div className="grid cols-3">
        <StatCard label="Estimated monthly cost" value={loading ? "—" : usd(storageCost)} hint={`${storedTB.toFixed(2)} TB × ${usd(pricePerTB)}/TB`} />
        <StatCard label="Stored volume" value={usage ? formatBytes(usage.storageBytes) : "—"} hint={`${storedTB.toFixed(3)} TB`} />
        <StatCard label="Egress this month" value={usage ? formatBytes(usage.egressBytesThisMonth) : "—"} hint={`Budget ${egressBudgetTB} TB`} />
      </div>

      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Component</th>
              <th>Basis</th>
              <th style={{ textAlign: "right" }}>Monthly estimate</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr><td colSpan={3}>Loading…</td></tr>
            ) : (
              <>
                <tr>
                  <td>Wasabi storage + Linode compute + AWS control plane</td>
                  <td className="muted">{storedTB.toFixed(3)} TB × {usd(pricePerTB)}/TB (all-in)</td>
                  <td style={{ textAlign: "right" }}>{usd(storageCost)}</td>
                </tr>
                <tr>
                  <td>Egress (within fair-use budget)</td>
                  <td className="muted">{Math.min(egressTB, egressBudgetTB).toFixed(3)} TB of {egressBudgetTB} TB · bundled</td>
                  <td style={{ textAlign: "right" }}>{usd(0)}</td>
                </tr>
                <tr>
                  <td>Egress overage</td>
                  <td className="muted">
                    {egressOverage > 0
                      ? `${egressOverage.toFixed(3)} TB over the 1× fair-use limit`
                      : "Within fair-use limit"}
                  </td>
                  <td style={{ textAlign: "right" }} className={egressOverage > 0 ? "danger-text" : "muted"}>
                    {egressOverage > 0 ? "review" : usd(0)}
                  </td>
                </tr>
                <tr>
                  <td><strong>Total</strong></td>
                  <td />
                  <td style={{ textAlign: "right" }}><strong>{usd(storageCost)}</strong></td>
                </tr>
              </>
            )}
          </tbody>
        </table>
      </div>
      {!loading && !tier && (
        <div className="muted" style={{ fontSize: 12 }}>
          No tier price found for license tier “{tenant?.licenseTier}”; storage cost shown as $0.
        </div>
      )}
    </div>
  );
}

// usd formats a dollar amount with two decimals and thousands
// separators.
function usd(amount: number): string {
  if (!Number.isFinite(amount)) return "—";
  return `$${amount.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`;
}

function StatCard({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="panel">
      <div className="muted" style={{ fontSize: 12, textTransform: "uppercase" }}>
        {label}
      </div>
      <div style={{ fontSize: 28, fontWeight: 700, marginTop: 4 }}>{value}</div>
      {hint && (
        <div className="muted" style={{ fontSize: 12, marginTop: 4 }}>
          {hint}
        </div>
      )}
    </div>
  );
}
