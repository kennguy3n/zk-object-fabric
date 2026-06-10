import { Activity, Database, Gauge as GaugeIcon, Send } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { api, usageSnapshotFromCounters } from "../api/client";
import { opsGet, type OpsCacheStats } from "../api/opsClient";
import type { CostBreakdown, UsageSnapshot } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { type ChartDatum, CHART_COLORS, DonutChart, TrendAreaChart, type TrendPoint } from "../components/charts";
import { GaugeChart } from "../components/GaugeChart";
import { PageHeader } from "../components/PageHeader";
import { StatCard } from "../components/StatCard";
import { EmptyState } from "../components/states";
import { formatBytes, formatNumber, formatPercent, formatUSD } from "../format";
import { Badge } from "../ui/badge";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "../ui/card";

// Server-Sent Events frame emitted by api/console/sse_handler.go.
// Counter names mirror billing.Dimension constants on the backend.
// It is a UsageCountersPayload plus an observed_at timestamp, so it
// projects onto a UsageSnapshot through the same
// usageSnapshotFromCounters() the REST bootstrap uses — no duplicate
// projection to keep in sync.
interface UsageStreamEvent {
  tenant_id: string;
  observed_at: string;
  start: string;
  end: string;
  counters: Record<string, number>;
}

// REQUEST_VERBS maps the per-verb counter dimensions onto a stable
// label + palette slot for the request-mix donut.
const REQUEST_VERBS: { key: string; label: string; color: string }[] = [
  { key: "get_requests", label: "GET", color: CHART_COLORS[0] },
  { key: "put_requests", label: "PUT", color: CHART_COLORS[1] },
  { key: "list_requests", label: "LIST", color: CHART_COLORS[2] },
  { key: "delete_requests", label: "DELETE", color: CHART_COLORS[3] },
];

const MAX_TREND_POINTS = 30;

export function DashboardPage() {
  const { tenant, token } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [streaming, setStreaming] = useState(false);
  const [cache, setCache] = useState<OpsCacheStats | null>(null);
  const [cost, setCost] = useState<CostBreakdown | null>(null);
  const [trend, setTrend] = useState<TrendPoint[]>([]);
  // loading is true only until the first usage snapshot resolves; the
  // SSE stream then keeps the figures live.
  const [loading, setLoading] = useState(true);
  const seededTrend = useRef(false);

  function pushTrendPoint(snapshot: UsageSnapshot) {
    setTrend((prev) => {
      const next = [
        ...prev,
        { t: new Date().toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }), value: snapshot.requestsLast30Days },
      ];
      return next.slice(-MAX_TREND_POINTS);
    });
  }

  useEffect(() => {
    let cancelled = false;
    api
      .currentUsage()
      .then((u) => {
        if (cancelled) return;
        setUsage(u);
        if (!seededTrend.current) {
          seededTrend.current = true;
          pushTrendPoint(u);
        }
      })
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, []);

  // Cache stats + cost breakdown are admin-gated; opsGet/costBreakdown
  // degrade to null rather than failing the dashboard.
  useEffect(() => {
    let cancelled = false;
    opsGet<OpsCacheStats>("cache-stats", token).then((c) => !cancelled && setCache(c));
    api.costBreakdown().then((c) => !cancelled && setCost(c)).catch(() => {});
    return () => {
      cancelled = true;
    };
  }, [token]);

  // Subscribe to the SSE usage stream once we know the tenant ID.
  useEffect(() => {
    if (!tenant?.id) return;
    if (typeof EventSource === "undefined") return;
    if (!token) return;
    // EventSource cannot set an Authorization header, so the SSE
    // endpoint accepts the bearer token on the query string. The
    // backend (sse_handler.go) validates the token and rejects
    // tokens bound to a different tenant than the URL path.
    const url = `/api/v1/usage/stream/${encodeURIComponent(tenant.id)}?token=${encodeURIComponent(token)}`;
    const es = new EventSource(url, { withCredentials: false });
    setStreaming(true);
    const onUsage = (ev: MessageEvent) => {
      try {
        const frame = JSON.parse(ev.data) as UsageStreamEvent;
        const snapshot = usageSnapshotFromCounters(frame);
        setUsage(snapshot);
        pushTrendPoint(snapshot);
        setError(null);
        setStreaming(true);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
      }
    };
    const onError = () => {
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
  }, [tenant?.id, token]);

  const requestMix = useMemo<ChartDatum[]>(() => {
    const c = usage?.counters ?? {};
    return REQUEST_VERBS.map((v) => ({ label: v.label, value: c[v.key] ?? 0, color: v.color })).filter(
      (d) => d.value > 0,
    );
  }, [usage]);

  const costMix = useMemo<ChartDatum[]>(() => {
    if (!cost) return [];
    return [
      { label: "Wasabi storage", value: cost.wasabi_storage_usd, color: CHART_COLORS[0] },
      { label: "Linode compute", value: cost.linode_compute_usd, color: CHART_COLORS[1] },
      { label: "AWS control plane", value: cost.aws_control_plane_usd, color: CHART_COLORS[2] },
    ].filter((d) => d.value > 0);
  }, [cost]);

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dashboard"
        description={`Live storage, request, and cost signals for ${tenant?.name ?? "your tenant"}.`}
        actions={
          <Badge variant={streaming ? "success" : "neutral"}>
            <span className={streaming ? "size-1.5 rounded-full bg-success" : "size-1.5 rounded-full bg-muted-foreground"} />
            {streaming ? "Live" : "Paused"}
          </Badge>
        }
      />

      {error && (
        <div className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning">
          {error}
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <StatCard label="Storage" value={usage ? formatBytes(usage.storageBytes) : "—"} icon={Database} loading={loading} />
        <StatCard
          label="Requests (window)"
          value={usage ? formatNumber(usage.requestsLast30Days) : "—"}
          icon={Activity}
          loading={loading}
        />
        <StatCard
          label="Egress this month"
          value={usage ? formatBytes(usage.egressBytesThisMonth) : "—"}
          hint={tenant ? `Budget: ${tenant.budgets.egressTbMonth} TiB/mo` : undefined}
          icon={Send}
          loading={loading}
        />
        <StatCard
          label="Cache hit ratio"
          value={cache ? formatPercent(cache.hitRatio) : "—"}
          hint={cache ? `${formatNumber(cache.hits)} hits · ${formatNumber(cache.misses)} misses` : "Operator access required"}
          icon={GaugeIcon}
          tone={cache && cache.hitRatio < 0.7 ? "warning" : "default"}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Requests in rolling window</CardTitle>
            <CardDescription>
              Cumulative request count over the rolling usage window, sampled live from the metering stream — a running level, not a per-second rate.
            </CardDescription>
          </CardHeader>
          <CardContent>
            {trend.length <= 1 ? (
              <EmptyState
                icon={Activity}
                title="Waiting for live samples"
                description="The trend fills in as the metering stream delivers usage frames."
              />
            ) : (
              <TrendAreaChart data={trend} format={formatNumber} />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Request mix</CardTitle>
            <CardDescription>Share by S3 verb over the usage window.</CardDescription>
          </CardHeader>
          <CardContent>
            {requestMix.length === 0 ? (
              <EmptyState icon={Activity} title="No requests yet" description="Verb breakdown appears once traffic is recorded." />
            ) : (
              <DonutChart
                data={requestMix}
                format={formatNumber}
                centerValue={formatNumber(usage?.requestsLast30Days ?? 0)}
                centerLabel="requests"
              />
            )}
          </CardContent>
        </Card>
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle>Cost breakdown per backend</CardTitle>
            <CardDescription>
              {cost
                ? `Provider split for ${cost.month}. Dedup saved ${formatUSD(cost.dedup_savings_usd)} this month.`
                : "Per-provider costs require operator access; see Billing for your tier estimate."}
            </CardDescription>
          </CardHeader>
          <CardContent>
            {costMix.length === 0 ? (
              <EmptyState
                icon={Database}
                title="Breakdown unavailable"
                description="The per-provider cost feed is operator-gated. The Billing screen always shows your usage-based estimate."
              />
            ) : (
              <DonutChart data={costMix} format={formatUSD} centerValue={formatUSD(cost?.total_usd ?? 0)} centerLabel="total" />
            )}
          </CardContent>
        </Card>

        <Card className="flex flex-col">
          <CardHeader>
            <CardTitle>Cache efficiency</CardTitle>
            <CardDescription>Hot-object cache hit ratio.</CardDescription>
          </CardHeader>
          <CardContent className="flex flex-1 items-center justify-center">
            {cache ? (
              <GaugeChart
                value={cache.hitRatio}
                label="Cache hit ratio"
                color={cache.hitRatio >= 0.9 ? "hsl(var(--zk-success))" : cache.hitRatio >= 0.7 ? "hsl(var(--zk-warning))" : "hsl(var(--zk-destructive))"}
              />
            ) : (
              <EmptyState icon={GaugeIcon} title="Cache stats unavailable" description="Operator access required." />
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
