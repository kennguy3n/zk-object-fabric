import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { UsageSnapshot } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";

// Server-Sent Events frame emitted by api/console/sse_handler.go.
// Counter names mirror billing.Dimension constants on the backend.
interface UsageStreamEvent {
  tenant_id: string;
  observed_at: string;
  start: string;
  end: string;
  counters: Record<string, number>;
}

// usageFromStreamEvent projects the counter map onto the
// UsageSnapshot shape the dashboard already renders so the live SSE
// frame can drop into the same StatCard without duplicating format
// logic.
function usageFromStreamEvent(ev: UsageStreamEvent): UsageSnapshot {
  const c = ev.counters ?? {};
  return {
    tenantId: ev.tenant_id,
    storageBytes: c["storage_bytes_seconds"] ?? 0,
    requestsLast30Days:
      (c["put_requests"] ?? 0) +
      (c["get_requests"] ?? 0) +
      (c["list_requests"] ?? 0) +
      (c["delete_requests"] ?? 0),
    egressBytesThisMonth: c["egress_bytes"] ?? 0,
    monthStart: ev.start,
  };
}

export function DashboardPage() {
  const { tenant } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [streaming, setStreaming] = useState(false);

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

  // Subscribe to the SSE usage stream once we know the tenant ID.
  // EventSource is a native browser API; we keep the connection open
  // for the lifetime of the dashboard and close it on unmount to
  // avoid leaking tabs in the React dev overlay.
  useEffect(() => {
    if (!tenant?.id) return;
    if (typeof EventSource === "undefined") return;
    const url = `/api/v1/usage/stream/${encodeURIComponent(tenant.id)}`;
    const es = new EventSource(url, { withCredentials: false });
    setStreaming(true);
    const onUsage = (ev: MessageEvent) => {
      try {
        const frame = JSON.parse(ev.data) as UsageStreamEvent;
        setUsage(usageFromStreamEvent(frame));
        setError(null);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    };
    const onError = () => {
      // Browser auto-reconnects on transport errors; surface the
      // most recent one to the operator so a stuck stream is
      // visible without flipping the entire dashboard red.
      setError("usage stream connection lost; reconnecting…");
      setStreaming(false);
    };
    es.addEventListener("usage", onUsage as EventListener);
    es.addEventListener("error", onError as EventListener);
    return () => {
      es.removeEventListener("usage", onUsage as EventListener);
      es.removeEventListener("error", onError as EventListener);
      es.close();
      setStreaming(false);
    };
  }, [tenant?.id]);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>
        Dashboard{" "}
        {streaming && (
          <span className="badge accent" style={{ fontSize: 12, verticalAlign: "middle" }}>
            live
          </span>
        )}
      </h1>
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
