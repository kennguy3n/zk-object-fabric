import { CalendarClock, Coins, Database, TrendingUp } from "lucide-react";

import { api } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { DonutChart, type ChartDatum } from "../components/charts";
import { PageHeader } from "../components/PageHeader";
import { StatCard } from "../components/StatCard";
import { CardError, EmptyState, InlineError } from "../components/states";
import { TierTable } from "../components/TierTable";
import { formatBytes, formatUSD } from "../format";
import { useAsync } from "../hooks/useAsync";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

// BillingPage estimates the tenant's monthly bill from live usage and
// the tier price book (both reachable from the tenant session), and
// overlays the operator-gated per-backend cost breakdown when available.
//
// The product bundles Wasabi storage + Linode compute + AWS control
// plane into one per-TiB tier price, so the headline estimate is
// stored volume × that all-in rate. In-budget egress is $0 incremental
// (Wasabi fair-use bundles egress up to 1× stored volume).

const BYTES_PER_TIB = 2 ** 40;

export function BillingPage() {
  const { tenant } = useAuth();
  // currentUsage + the tier price book are the required inputs for the
  // headline usage×tier estimate, which is always tenant-accessible.
  const { data, loading, error, reload } = useAsync(
    () => Promise.all([api.currentUsage(), api.listTierConfigs()]),
    [],
  );
  const usage = data?.[0] ?? null;
  const tiers = data?.[1] ?? [];

  // The per-backend cost breakdown is admin-gated, so it loads on its
  // own: a gated tenant gets a null breakdown (handled by client.ts as
  // 401/403/404/503 -> null) and still sees the full estimate, while a
  // genuine backend fault (5xx / network) surfaces as an error in just
  // the breakdown card rather than being silently swallowed or taking
  // down the whole page.
  const { data: breakdown, loading: breakdownLoading, error: breakdownError } = useAsync(
    () => api.costBreakdown(),
    [],
  );

  const tier = tiers.find((t) => t.tier === tenant?.licenseTier);
  // A tenant whose licenseTier is absent from the price book (e.g. the
  // beta tier, which DefaultTierConfigs omits) falls through to a $0
  // estimate. Surface that explicitly so $0 reads as "not priced yet",
  // not "free".
  const priceMissing = !loading && !error && tiers.length > 0 && !!tenant?.licenseTier && !tier;
  const storedTB = (usage?.storageBytes ?? 0) / BYTES_PER_TIB;
  const egressTB = (usage?.egressBytesThisMonth ?? 0) / BYTES_PER_TIB;
  const pricePerTB = tier?.price_per_tb_month ?? 0;
  const storageCost = storedTB * pricePerTB;
  const egressBudgetTB = tenant?.budgets.egressTbMonth ?? 0;
  const egressOverage = Math.max(egressTB - egressBudgetTB, 0);

  // Storage cost is a level, not a flow: the monthly bill is
  // stored volume × per-TiB rate regardless of the day of the month
  // (stored bytes do not accumulate over the month the way egress
  // does). storageBytes is already the average volume held over the
  // usage window (see usageSnapshotFromCounters in client.ts), so
  // storageCost is the current monthly run-rate and the 12-month
  // projection simply assumes that volume holds flat — no proration.
  const annualForecast = storageCost * 12;

  const breakdownData: ChartDatum[] = breakdown
    ? [
        { label: "Wasabi storage", value: breakdown.wasabi_storage_usd },
        { label: "Linode compute", value: breakdown.linode_compute_usd },
        { label: "AWS control plane", value: breakdown.aws_control_plane_usd },
      ].filter((d) => d.value > 0)
    : [];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Billing"
        description={
          <>
            Estimated monthly cost for <span className="font-medium text-foreground">{tenant?.name}</span> on the{" "}
            <span className="font-medium text-foreground">{tier?.display_name ?? tenant?.licenseTier}</span> tier. The per-TiB rate
            bundles Wasabi storage, Linode compute, and the AWS control plane.
          </>
        }
      />

      {error && <CardError message={`Failed to load cost inputs: ${error}`} onRetry={reload} />}

      {priceMissing && (
        <div className="rounded-md border border-warning/40 bg-warning/10 px-3 py-2 text-sm text-warning">
          No tier price found for license tier “{tenant?.licenseTier}”; storage cost is shown as {formatUSD(0)} until this tier is added to the price book.
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Estimated this month" value={formatUSD(storageCost)} hint={`${storedTB.toFixed(2)} TiB × ${formatUSD(pricePerTB)}/TiB`} icon={Coins} loading={loading} />
        <StatCard label="Projected annual" value={formatUSD(annualForecast)} hint="At current run-rate (flat volume)" icon={CalendarClock} loading={loading} />
        <StatCard label="Stored volume" value={usage ? formatBytes(usage.storageBytes) : "—"} hint={`${storedTB.toFixed(3)} TiB`} icon={Database} loading={loading} />
        <StatCard label="Egress this month" value={usage ? formatBytes(usage.egressBytesThisMonth) : "—"} hint={`Budget ${egressBudgetTB} TiB`} icon={TrendingUp} loading={loading} tone={egressOverage > 0 ? "destructive" : "default"} />
      </div>

      <div className="grid gap-6 lg:grid-cols-[1fr_minmax(280px,360px)]">
        <Card>
          <CardHeader>
            <CardTitle>Cost components</CardTitle>
          </CardHeader>
          <CardContent className="p-0">
            {loading ? (
              <div className="space-y-2 p-4">{[0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-10 w-full" />)}</div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Component</TableHead>
                    <TableHead>Basis</TableHead>
                    <TableHead className="text-right">Monthly estimate</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  <TableRow>
                    <TableCell className="font-medium text-foreground">Storage + compute + control plane</TableCell>
                    <TableCell className="text-muted-foreground">{storedTB.toFixed(3)} TiB × {formatUSD(pricePerTB)}/TiB (all-in)</TableCell>
                    <TableCell className="text-right tabular-nums">{formatUSD(storageCost)}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell className="font-medium text-foreground">Egress (within fair-use)</TableCell>
                    <TableCell className="text-muted-foreground">{Math.min(egressTB, egressBudgetTB).toFixed(3)} of {egressBudgetTB} TiB · bundled</TableCell>
                    <TableCell className="text-right tabular-nums">{formatUSD(0)}</TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell className="font-medium text-foreground">Egress overage</TableCell>
                    <TableCell className="text-muted-foreground">
                      {egressOverage > 0 ? `${egressOverage.toFixed(3)} TiB over the 1× fair-use limit` : "Within fair-use limit"}
                    </TableCell>
                    <TableCell className={`text-right tabular-nums ${egressOverage > 0 ? "text-destructive" : "text-muted-foreground"}`}>
                      {egressOverage > 0 ? "review" : formatUSD(0)}
                    </TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell className="font-semibold text-foreground">Total</TableCell>
                    <TableCell />
                    <TableCell className="text-right font-semibold tabular-nums text-foreground">{formatUSD(storageCost)}</TableCell>
                  </TableRow>
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Cost per backend</CardTitle>
          </CardHeader>
          <CardContent>
            {breakdownLoading ? (
              <Skeleton className="h-[220px] w-full" />
            ) : breakdownError ? (
              <InlineError message={`Cost breakdown failed: ${breakdownError}`} />
            ) : breakdown && breakdownData.length > 0 ? (
              <>
                <DonutChart data={breakdownData} format={formatUSD} centerValue={formatUSD(breakdown.total_usd)} centerLabel="total" />
                {breakdown.dedup_savings_usd > 0 && (
                  <p className="mt-3 text-center text-xs text-success">Dedup saved {formatUSD(breakdown.dedup_savings_usd)} this month</p>
                )}
              </>
            ) : (
              <EmptyState icon={Coins} title="Backend breakdown unavailable" description="Per-backend cost attribution requires operator access. Your all-in estimate is shown on the left." />
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Tier comparison</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {loading ? (
            <div className="space-y-2 p-4">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
          ) : tiers.length === 0 ? (
            <EmptyState title="No tiers configured" />
          ) : (
            <TierTable tiers={tiers} activeTier={tenant?.licenseTier} />
          )}
        </CardContent>
      </Card>
    </div>
  );
}
