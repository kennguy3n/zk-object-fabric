import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { UsageSnapshot } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";

export function DashboardPage() {
  const { tenant } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    api
      .currentUsage()
      .then((u) => !cancelled && setUsage(u))
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)));
    return () => {
      cancelled = true;
    };
  }, []);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Dashboard</h1>
      {error && <div className="panel danger-text">Failed to load usage: {error}</div>}
      <div className="grid cols-3">
        <StatCard
          label="Storage"
          value={usage ? formatBytes(usage.storageBytes) : "—"}
        />
        <StatCard
          label="Requests (last 30d)"
          value={usage ? usage.requestsLast30Days.toLocaleString() : "—"}
        />
        <StatCard
          label="Egress this month"
          value={usage ? formatBytes(usage.egressBytesThisMonth) : "—"}
          hint={tenant ? `Budget: ${tenant.budgets.egressTbMonth} TB/mo` : undefined}
        />
      </div>
      <div className="panel">
        <div className="muted" style={{ fontSize: 13, marginBottom: 8 }}>
          Tenant
        </div>
        <div style={{ fontWeight: 600 }}>{tenant?.name}</div>
        <div className="muted" style={{ fontSize: 13 }}>
          Contract: {tenant?.contractType} · License: {tenant?.licenseTier} · Default
          placement: {tenant?.placementDefaultPolicyRef}
        </div>
      </div>
    </div>
  );
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
