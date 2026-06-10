import { Activity, HeartPulse, ShieldAlert } from "lucide-react";
import { useEffect, useState } from "react";

import { api } from "../api/client";
import type { UsageSnapshot } from "../api/types";
import { opsGet, type OpsCacheStats, type OpsHealth } from "../api/opsClient";
import { useAuth } from "../auth/AuthContext";
import { GaugeChart } from "../components/GaugeChart";
import { PageHeader } from "../components/PageHeader";
import { EmptyState, InlineError } from "../components/states";
import { formatBytes, formatNumber } from "../format";
import { gaugeColor } from "../lib/gauge";
import { Badge } from "../ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Progress } from "../ui/progress";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableRow } from "../ui/table";

// OperationsPage is the built-in ops view for SME operators who do not
// run Grafana/Prometheus. It aggregates the gateway's own signals —
// node health, hot-object-cache hit ratio, per-tenant storage/egress
// usage, and the Wasabi fair-use egress budget.
//
// GET /api/v1/usage is always available to the console session; the
// /api/v1/ops/* endpoints are admin-gated and degrade gracefully.

// egressTbMonth is a binary tebibyte, matching the gateway guardrail
// (internal/auth/abuse.go uses 1<<40) and formatBytes' binary prefixes.
const BYTES_PER_TIB = 2 ** 40;

export function OperationsPage() {
  const { tenant, token } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [health, setHealth] = useState<OpsHealth | null>(null);
  const [cache, setCache] = useState<OpsCacheStats | null>(null);
  const [usageError, setUsageError] = useState<string | null>(null);
  const [usageLoading, setUsageLoading] = useState(true);
  const [opsLoaded, setOpsLoaded] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api
      .currentUsage()
      .then((u) => !cancelled && setUsage(u))
      .catch((e) => !cancelled && setUsageError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setUsageLoading(false));
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
  const storageBytes = usage?.storageBytes ?? 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Operations"
        description={`Gateway health, cache efficiency, and Wasabi fair-use budget for ${tenant?.name ?? "your tenant"}. No Grafana required.`}
      />

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>Gateway health</CardTitle>
            <HeartPulse className="size-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            {!opsLoaded ? (
              <Skeleton className="h-16 w-full" />
            ) : health ? (
              <div className="space-y-1">
                <Badge variant={health.status === "ok" ? "success" : "destructive"}>
                  {health.status === "ok" ? "Healthy" : "Degraded"}
                </Badge>
                <div className="pt-1 text-xs text-muted-foreground">
                  {health.node.nodeId}
                  {health.node.cellId ? ` · ${health.node.cellId}` : ""} · state {health.node.state}
                </div>
                <div className="text-xs text-muted-foreground">
                  {health.node.inflight} in-flight · quorum ≥ {health.node.quorumThreshold}
                </div>
              </div>
            ) : (
              <p className="text-sm text-muted-foreground">Health feed unavailable (admin access required).</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Cache hit ratio</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center justify-center">
            {!opsLoaded ? (
              <Skeleton className="h-24 w-40" />
            ) : cache ? (
              <GaugeChart value={cache.hitRatio} label="Cache hit ratio" color={gaugeColor(cache.hitRatio, "higher-better")} />
            ) : (
              <p className="text-sm text-muted-foreground">Unavailable (admin access required).</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Wasabi egress budget</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center justify-center">
            {usageLoading ? (
              <Skeleton className="h-24 w-40" />
            ) : (
              <GaugeChart value={egressConsumed} label="Egress budget used" color={gaugeColor(egressConsumed, "lower-better")} />
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Storage & egress usage</CardTitle>
        </CardHeader>
        <CardContent className="space-y-5">
          {usageError && <InlineError message={`Failed to load usage: ${usageError}`} />}
          {usageLoading ? (
            <Skeleton className="h-20 w-full" />
          ) : (
            <>
              <UsageBar label="Stored" value={storageBytes} max={Math.max(storageBytes, egressBudgetBytes, 1)} display={formatBytes(storageBytes)} />
              <UsageBar
                label="Egress this month"
                value={egressUsed}
                max={Math.max(egressBudgetBytes, egressUsed, 1)}
                display={`${formatBytes(egressUsed)} / ${tenant?.budgets.egressTbMonth ?? 0} TiB budget`}
                danger={egressConsumed >= 0.95}
              />
              <p className="text-xs text-muted-foreground">
                Egress budget remaining: <span className="font-medium text-foreground">{formatBytes(egressRemaining)}</span>
              </p>
            </>
          )}
        </CardContent>
      </Card>

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle>Cache detail</CardTitle>
          </CardHeader>
          <CardContent>
            {!opsLoaded ? (
              <Skeleton className="h-32 w-full" />
            ) : cache ? (
              <Table>
                <TableBody>
                  <DetailRow label="Entries" value={formatNumber(cache.entries)} />
                  <DetailRow label="Used" value={formatBytes(cache.bytesUsed)} />
                  <DetailRow label="Limit" value={formatBytes(cache.bytesLimit)} />
                  <DetailRow label="Hits / misses" value={`${formatNumber(cache.hits)} / ${formatNumber(cache.misses)}`} />
                  <DetailRow label="Evictions" value={formatNumber(cache.evictions)} />
                </TableBody>
              </Table>
            ) : (
              <p className="text-sm text-muted-foreground">Unavailable.</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>Abuse-detector alerts</CardTitle>
            <ShieldAlert className="size-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <EmptyState icon={ShieldAlert} title="No active alerts" description="Active rate-limit / abuse-detector trips for this tenant appear here." />
          </CardContent>
        </Card>

        <Card>
          <CardHeader className="flex-row items-center justify-between">
            <CardTitle>Lifecycle actions</CardTitle>
            <Activity className="size-4 text-muted-foreground" />
          </CardHeader>
          <CardContent>
            <EmptyState icon={Activity} title="No recent actions" description="Tier transitions and expirations applied by the lifecycle evaluator appear here." />
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

function DetailRow({ label, value }: { label: string; value: string }) {
  return (
    <TableRow>
      <TableCell className="text-muted-foreground">{label}</TableCell>
      <TableCell className="text-right font-medium tabular-nums text-foreground">{value}</TableCell>
    </TableRow>
  );
}

function UsageBar({ label, value, max, display, danger }: { label: string; value: number; max: number; display: string; danger?: boolean }) {
  const pct = max > 0 ? Math.min((value / max) * 100, 100) : 0;
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between text-sm">
        <span className="text-foreground">{label}</span>
        <span className="text-muted-foreground">{display}</span>
      </div>
      <Progress value={pct} indicatorClassName={danger ? "bg-destructive" : undefined} />
    </div>
  );
}
