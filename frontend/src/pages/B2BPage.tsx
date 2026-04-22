import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { DedicatedCell } from "../api/types";

export function B2BPage() {
  const [cells, setCells] = useState<DedicatedCell[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    api
      .listDedicatedCells()
      .then((c) => !cancelled && setCells(c))
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Dedicated cells</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Dedicated cells are provisioned by operations. Use this page to review
        the cells assigned to your contract and their current utilization.
      </div>
      {error && <div className="panel danger-text">{error}</div>}
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Cell ID</th>
              <th>Region</th>
              <th>Country</th>
              <th>Status</th>
              <th>Capacity</th>
              <th>Utilization</th>
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={6} className="muted">
                  Loading…
                </td>
              </tr>
            )}
            {!loading && cells.length === 0 && (
              <tr>
                <td colSpan={6} className="muted">
                  No dedicated cells provisioned. Contact your account manager
                  to request one.
                </td>
              </tr>
            )}
            {cells.map((c) => (
              <tr key={c.id}>
                <td style={{ fontFamily: "monospace" }}>{c.id}</td>
                <td>{c.region}</td>
                <td>{c.country}</td>
                <td>
                  <span className={`badge ${c.status === "active" ? "accent" : ""}`}>
                    {c.status}
                  </span>
                </td>
                <td>{c.capacityPetabytes.toFixed(1)} PB</td>
                <td>{Math.round(c.utilization * 100)}%</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}
