import { Server, ServerCog } from "lucide-react";
import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { DedicatedCell, ProvisionCellInput } from "../api/types";
import { PageHeader } from "../components/PageHeader";
import { StatCard } from "../components/StatCard";
import { CardError, EmptyState, InlineError } from "../components/states";
import { formatPercent } from "../format";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "../ui/dialog";
import { Input } from "../ui/input";
import { Label } from "../ui/label";
import { Progress } from "../ui/progress";
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { useToast } from "../ui/toast";

const STATUS_VARIANT: Record<DedicatedCell["status"], "default" | "success" | "warning" | "neutral"> = {
  active: "success",
  provisioning: "warning",
  decommissioning: "neutral",
};

// CellsPage is the B2B dedicated-cell view: a health dashboard over the
// tenant's provisioned cells (GET /api/v1/dedicated-cells) plus a
// provisioning request form (POST /api/v1/tenants/{id}/dedicated-cells).
// Provisioning is operator-gated; the form surfaces backend errors
// rather than pretending a cell appeared.
export function CellsPage() {
  const { toast } = useToast();
  const [cells, setCells] = useState<DedicatedCell[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [open, setOpen] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setCells(await api.listDedicatedCells());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const active = cells.filter((c) => c.status === "active").length;
  const totalCapacity = cells.reduce((s, c) => s + c.capacityPetabytes, 0);
  const avgUtil = cells.length > 0 ? cells.reduce((s, c) => s + c.utilization, 0) / cells.length : 0;

  return (
    <div className="space-y-6">
      <PageHeader
        title="Dedicated cells"
        description="Isolated storage cells provisioned for your contract. Review health and request additional capacity."
        actions={
          <Button onClick={() => setOpen(true)}>
            <ServerCog className="size-4" /> Request cell
          </Button>
        }
      />

      <div className="grid gap-4 sm:grid-cols-3">
        <StatCard label="Active cells" value={`${active} / ${cells.length}`} icon={Server} loading={loading} />
        <StatCard label="Total capacity" value={`${totalCapacity.toFixed(1)} PB`} loading={loading} />
        <StatCard label="Avg utilization" value={formatPercent(avgUtil, 0)} loading={loading} tone={avgUtil >= 0.85 ? "warning" : "default"} />
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Cells</CardTitle>
        </CardHeader>
        <CardContent className="p-0">
          {loading ? (
            <div className="space-y-2 p-4">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-10 w-full" />)}</div>
          ) : error ? (
            <CardError message={error} onRetry={() => void refresh()} />
          ) : cells.length === 0 ? (
            <EmptyState
              icon={Server}
              title="No dedicated cells"
              description="Request a dedicated cell to get isolated, single-tenant storage capacity."
              action={<Button onClick={() => setOpen(true)}><ServerCog className="size-4" /> Request cell</Button>}
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Cell ID</TableHead>
                  <TableHead>Region</TableHead>
                  <TableHead>Country</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead className="text-right">Capacity</TableHead>
                  <TableHead className="w-[180px]">Utilization</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {cells.map((c) => (
                  <TableRow key={c.id}>
                    <TableCell className="font-mono text-xs">{c.id}</TableCell>
                    <TableCell>{c.region}</TableCell>
                    <TableCell>{c.country}</TableCell>
                    <TableCell><Badge variant={STATUS_VARIANT[c.status]}>{c.status}</Badge></TableCell>
                    <TableCell className="text-right tabular-nums">{c.capacityPetabytes.toFixed(1)} PB</TableCell>
                    <TableCell>
                      <div className="flex items-center gap-2">
                        <Progress value={c.utilization * 100} indicatorClassName={c.utilization >= 0.85 ? "bg-warning" : undefined} />
                        <span className="w-10 shrink-0 text-right text-xs tabular-nums text-muted-foreground">{formatPercent(c.utilization, 0)}</span>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <ProvisionDialog open={open} onOpenChange={setOpen} onProvisioned={() => { void refresh(); toast({ title: "Provisioning requested", description: "The cell will appear as it comes online.", variant: "success" }); }} />
    </div>
  );
}

const DEFAULT_INPUT: ProvisionCellInput = {
  region: "",
  country: "",
  capacityPetabytes: 1,
  erasureProfile: "rs-6-3",
  nodeCount: 6,
};

function ProvisionDialog({ open, onOpenChange, onProvisioned }: { open: boolean; onOpenChange: (o: boolean) => void; onProvisioned: () => void }) {
  const [input, setInput] = useState<ProvisionCellInput>(DEFAULT_INPUT);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  function set<K extends keyof ProvisionCellInput>(key: K, value: ProvisionCellInput[K]) {
    setInput((prev) => ({ ...prev, [key]: value }));
  }

  async function onSubmit(e: React.FormEvent) {
    e.preventDefault();
    setErr(null);
    setBusy(true);
    try {
      await api.provisionDedicatedCell({
        ...input,
        region: input.region.trim(),
        country: input.country.trim().toUpperCase(),
      });
      onOpenChange(false);
      setInput(DEFAULT_INPUT);
      onProvisioned();
    } catch (e2) {
      setErr(e2 instanceof Error ? e2.message : String(e2));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={onSubmit}>
          <DialogHeader>
            <DialogTitle>Request dedicated cell</DialogTitle>
            <DialogDescription>Provisioning is fulfilled by operations. You'll be billed per the dedicated tier once the cell is active.</DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-2 gap-4 py-4">
            <div className="space-y-1.5">
              <Label htmlFor="region">Region</Label>
              <Input id="region" value={input.region} onChange={(e) => set("region", e.target.value)} placeholder="eu-central" required autoFocus />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="country">Country (ISO)</Label>
              <Input id="country" value={input.country} onChange={(e) => set("country", e.target.value)} placeholder="DE" maxLength={2} required />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="capacity">Capacity (PB)</Label>
              <Input id="capacity" type="number" min={1} step={1} value={input.capacityPetabytes} onChange={(e) => set("capacityPetabytes", Number(e.target.value))} required />
            </div>
            <div className="space-y-1.5">
              <Label htmlFor="nodes">Node count</Label>
              <Input id="nodes" type="number" min={3} step={1} value={input.nodeCount} onChange={(e) => set("nodeCount", Number(e.target.value))} required />
            </div>
            <div className="col-span-2 space-y-1.5">
              <Label htmlFor="ec">Erasure profile</Label>
              <Input id="ec" value={input.erasureProfile ?? ""} onChange={(e) => set("erasureProfile", e.target.value)} placeholder="rs-6-3" />
              <p className="text-xs text-muted-foreground">Reed–Solomon data/parity split (e.g. rs-6-3 = 6 data + 3 parity shards).</p>
            </div>
          </div>
          {err && <InlineError message={err} className="mb-2" />}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">Cancel</Button>
            </DialogClose>
            <Button type="submit" disabled={busy}>{busy ? "Requesting…" : "Request cell"}</Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
