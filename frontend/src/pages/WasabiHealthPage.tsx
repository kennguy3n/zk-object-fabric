import { CloudCog } from "lucide-react";
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
import { GaugeChart } from "../components/GaugeChart";
import { PageHeader } from "../components/PageHeader";
import { EmptyState, InlineError } from "../components/states";
import { formatBytes, formatPercent } from "../format";
import { gaugeColor } from "../lib/gauge";
import { Badge } from "../ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

// WasabiHealthPage focuses on the Wasabi fair-use envelope: monthly
// origin egress ≤ 1× active stored bytes (providers/wasabi/guardrails.go).
// It shows where the tenant sits against that 1× limit and tracks cache
// warm-up, since a cold cache pushes reads to origin and burns budget.
//
// The egress-vs-stored ratio comes from GET /api/v1/usage; cache warm-up
// and the fleet-wide budget table come from the admin-gated ops surface.

export function WasabiHealthPage() {
  const { tenant, token } = useAuth();
  const [usage, setUsage] = useState<UsageSnapshot | null>(null);
  const [cache, setCache] = useState<OpsCacheStats | null>(null);
  const [budgets, setBudgets] = useState<WasabiBudget[] | null>(null);
  const [opsLoaded, setOpsLoaded] = useState(false);
  const [usageLoading, setUsageLoading] = useState(true);
  const [usageError, setUsageError] = useState<string | null>(null);

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
  const egressRatio = stored > 0 ? egress / stored : 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Wasabi health"
        description={`Fair-use posture for ${tenant?.name ?? "your tenant"}: monthly origin egress must stay within 1× active stored bytes. A warm cache keeps reads off Wasabi origin.`}
      />
      {usageError && <InlineError message={`Failed to load usage: ${usageError}`} />}

      <div className="grid gap-4 md:grid-cols-3">
        <Card>
          <CardHeader>
            <CardTitle>Egress ÷ 1× stored</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center justify-center">
            {usageLoading ? (
              <Skeleton className="h-24 w-40" />
            ) : (
              <GaugeChart
                value={egressRatio}
                label="Egress ÷ 1× stored"
                valueLabel={formatPercent(egressRatio, 0)}
                color={gaugeColor(egressRatio, "lower-better")}
              />
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Cache warm-up</CardTitle>
          </CardHeader>
          <CardContent className="flex items-center justify-center">
            {!opsLoaded ? (
              <Skeleton className="h-24 w-40" />
            ) : cache ? (
              <GaugeChart value={cache.utilization} label="Cache warm-up" color={gaugeColor(cache.utilization, "higher-better")} />
            ) : (
              <p className="text-sm text-muted-foreground">Cache feed unavailable.</p>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Fair-use summary</CardTitle>
          </CardHeader>
          <CardContent>
            {usageLoading ? (
              <Skeleton className="h-32 w-full" />
            ) : (
              <Table>
                <TableBody>
                  <SummaryRow label="Stored" value={formatBytes(stored)} />
                  <SummaryRow label="Egress (mo)" value={formatBytes(egress)} />
                  <SummaryRow label="1× limit" value={formatBytes(stored)} />
                  <TableRow>
                    <TableCell className="text-muted-foreground">Headroom</TableCell>
                    <TableCell className={`text-right font-medium tabular-nums ${egress > stored ? "text-destructive" : "text-success"}`}>
                      {formatBytes(Math.max(stored - egress, 0))}
                    </TableCell>
                  </TableRow>
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>90-day minimum-storage warnings</CardTitle>
        </CardHeader>
        <CardContent>
          <EmptyState
            icon={CloudCog}
            title="No early-deletion charges pending"
            description="Objects deleted before 90 days still incur Wasabi's minimum-storage charge. Recently-deleted objects within their window appear here."
          />
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle>Per-tenant egress budgets</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {!opsLoaded ? (
            <div className="space-y-2 p-4">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
          ) : budgets === null ? (
            <EmptyState title="Budget feed unavailable" description="Fleet-wide egress budgets require admin access." />
          ) : budgets.length === 0 ? (
            <EmptyState title="No tenants reporting Wasabi egress" />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Tenant</TableHead>
                  <TableHead className="text-right">Stored</TableHead>
                  <TableHead className="text-right">Egress</TableHead>
                  <TableHead className="text-right">Ratio</TableHead>
                  <TableHead className="text-right">Remaining</TableHead>
                  <TableHead>Status</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {budgets.map((b) => (
                  <TableRow key={b.tenantId}>
                    <TableCell className="font-mono text-xs">{b.tenantId}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatBytes(b.storedBytes)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatBytes(b.egressBytes)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatPercent(b.egressRatio, 0)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatBytes(b.remainingBytes)}</TableCell>
                    <TableCell>
                      <Badge variant={b.status === "ok" ? "success" : "warning"}>{b.status}</Badge>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  );
}

function SummaryRow({ label, value }: { label: string; value: string }) {
  return (
    <TableRow>
      <TableCell className="text-muted-foreground">{label}</TableCell>
      <TableCell className="text-right font-medium tabular-nums text-foreground">{value}</TableCell>
    </TableRow>
  );
}
