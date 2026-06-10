import type { LicenseTier, TierConfig } from "../api/types";
import { formatUSD } from "../format";
import { Badge } from "../ui/badge";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

// TierTable renders the canonical product-tier price book
// (GET /api/v1/tiers) as a comparison grid. The tenant's active tier is
// highlighted so Billing and Account can reuse one component to show
// "where you are" against the rest of the catalogue.
export function TierTable({
  tiers,
  activeTier,
}: {
  tiers: TierConfig[];
  activeTier?: LicenseTier;
}) {
  return (
    <div className="overflow-x-auto">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Tier</TableHead>
            <TableHead>EC profile</TableHead>
            <TableHead>Cache</TableHead>
            <TableHead>Dedup</TableHead>
            <TableHead>Placement</TableHead>
            <TableHead className="text-right">Egress / mo</TableHead>
            <TableHead className="text-right">Price / TiB-mo</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {tiers.map((t) => {
            const active = t.tier === activeTier;
            return (
              <TableRow key={t.tier} className={active ? "bg-primary/5" : undefined}>
                <TableCell>
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-foreground">{t.display_name}</span>
                    {active && <Badge variant="success">Current</Badge>}
                    {t.country_locked && <Badge variant="neutral">country-locked</Badge>}
                  </div>
                </TableCell>
                <TableCell className="text-muted-foreground">{t.default_ec_profile}</TableCell>
                <TableCell className="text-muted-foreground">{t.cache_policy}</TableCell>
                <TableCell className="text-muted-foreground">{t.dedup_policy}</TableCell>
                <TableCell className="text-muted-foreground">{t.placement_mode}</TableCell>
                <TableCell className="text-right tabular-nums">{t.egress_budget_tb_month} TiB</TableCell>
                <TableCell className="text-right tabular-nums">{formatUSD(t.price_per_tb_month)}</TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </div>
  );
}
