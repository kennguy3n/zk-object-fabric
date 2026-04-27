import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { TierConfig } from "../api/types";

// TiersPage renders the canonical product-tier comparison from
// GET /api/v1/tiers. The route is read-only; sales / SE staff
// link customers here to compare default EC, cache, dedup, and
// pricing knobs across the five license tiers.
export function TiersPage() {
  const [tiers, setTiers] = useState<TierConfig[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    api
      .listTierConfigs()
      .then((t) => !cancelled && setTiers(t))
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Product tiers</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Default storage, cache, dedup, and pricing for each license tier.
        These are the values applied to a new tenant unless an operator
        overrides them at signup.
      </div>
      {error && <div className="panel danger-text">{error}</div>}
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Tier</th>
              <th>EC profile</th>
              <th>Cache</th>
              <th>Dedup</th>
              <th>Placement</th>
              <th>Egress / mo</th>
              <th>Price / TB-mo</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={7}>Loading…</td>
              </tr>
            ) : tiers.length === 0 ? (
              <tr>
                <td colSpan={7}>No tiers configured.</td>
              </tr>
            ) : (
              tiers.map((t) => (
                <tr key={t.tier}>
                  <td>
                    <strong>{t.display_name}</strong>
                    {t.country_locked && (
                      <span className="muted" style={{ marginLeft: 6, fontSize: 12 }}>
                        country-locked
                      </span>
                    )}
                  </td>
                  <td>{t.default_ec_profile}</td>
                  <td>{t.cache_policy}</td>
                  <td>{t.dedup_policy}</td>
                  <td>{t.placement_mode}</td>
                  <td>{t.egress_budget_tb_month} TB</td>
                  <td>${t.price_per_tb_month.toFixed(2)}</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  );
}
