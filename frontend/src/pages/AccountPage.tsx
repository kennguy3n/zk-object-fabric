import { Building2, LogOut } from "lucide-react";

import { api } from "../api/client";
import { useAuth } from "../auth/AuthContext";
import { PageHeader } from "../components/PageHeader";
import { CardError, EmptyState } from "../components/states";
import { TierTable } from "../components/TierTable";
import { formatDateTime, formatNumber } from "../format";
import { useAsync } from "../hooks/useAsync";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Skeleton } from "../ui/skeleton";

const CONTRACT_LABEL: Record<string, string> = {
  b2c_pooled: "B2C · pooled",
  b2b_shared: "B2B · shared",
  b2b_dedicated: "B2B · dedicated",
  sovereign: "Sovereign",
};

// AccountPage surfaces the tenant identity, request/egress budgets, and
// the product tier catalogue (GET /api/v1/tiers) with the tenant's
// active tier highlighted, so operators can see where they sit and what
// upgrading would change. Tier changes are operator-driven, so the
// table is read-only by design.
export function AccountPage() {
  const { tenant, signOut } = useAuth();
  const { data, loading, error, reload } = useAsync(() => api.listTierConfigs(), []);
  const tiers = data ?? [];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Account"
        description="Your tenant identity, budgets, and product tier."
        actions={
          <Button variant="outline" onClick={signOut}>
            <LogOut className="size-4" /> Sign out
          </Button>
        }
      />

      <div className="grid gap-6 md:grid-cols-2">
        <Card>
          <CardHeader className="flex-row items-center gap-3">
            <span className="flex size-10 items-center justify-center rounded-lg bg-primary/15 text-primary">
              <Building2 className="size-5" />
            </span>
            <CardTitle>{tenant?.name ?? "Tenant"}</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="divide-y divide-border text-sm">
              <Row label="Tenant ID" value={<span className="font-mono text-xs">{tenant?.id}</span>} />
              <Row label="Contract" value={<Badge variant="neutral">{CONTRACT_LABEL[tenant?.contractType ?? ""] ?? tenant?.contractType}</Badge>} />
              <Row label="License tier" value={<Badge variant="default">{tenant?.licenseTier}</Badge>} />
              <Row label="Default placement" value={tenant?.placementDefaultPolicyRef || "—"} />
              <Row label="Created" value={formatDateTime(tenant?.createdAt)} />
            </dl>
          </CardContent>
        </Card>

        <Card>
          <CardHeader>
            <CardTitle>Budgets & guardrails</CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="divide-y divide-border text-sm">
              <Row label="Requests / sec" value={formatNumber(tenant?.budgets.requestsPerSec ?? 0)} />
              <Row label="Burst requests" value={formatNumber(tenant?.budgets.burstRequests ?? 0)} />
              <Row label="Egress budget" value={`${tenant?.budgets.egressTbMonth ?? 0} TiB / month`} />
            </dl>
            <p className="mt-4 text-xs text-muted-foreground">
              These limits protect the shared fabric. Egress fair-use is 1× active stored bytes per month; see the Wasabi health screen for live posture.
            </p>
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Product tiers</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {loading ? (
            <div className="space-y-2 p-4">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-9 w-full" />)}</div>
          ) : error ? (
            <CardError message={error} onRetry={reload} />
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

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-4 py-2.5">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="text-right font-medium text-foreground">{value}</dd>
    </div>
  );
}
