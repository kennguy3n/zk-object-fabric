import { Database, Plus, Settings2, Trash2 } from "lucide-react";
import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { Bucket, DedupPolicy } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { PageHeader } from "../components/PageHeader";
import { CardError, EmptyState, InlineError } from "../components/states";
import { formatBytes, formatNumber } from "../format";
import { useToast } from "../ui/toast";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardContent } from "../ui/card";
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
import { Skeleton } from "../ui/skeleton";
import { Switch } from "../ui/switch";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";

export function BucketsPage() {
  const { tenant } = useAuth();
  const { toast } = useToast();
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState("");
  const [policyRef, setPolicyRef] = useState(tenant?.placementDefaultPolicyRef ?? "");
  const [creating, setCreating] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  const [configBucket, setConfigBucket] = useState<Bucket | null>(null);
  const [deleteBucket, setDeleteBucket] = useState<Bucket | null>(null);
  const [deleting, setDeleting] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setBuckets(await api.listBuckets());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function onCreate(e: React.FormEvent) {
    e.preventDefault();
    setCreateError(null);
    setCreating(true);
    try {
      await api.createBucket(name.trim(), policyRef.trim());
      toast({ title: "Bucket created", description: name.trim(), variant: "success" });
      setName("");
      setCreateOpen(false);
      await refresh();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : String(err));
    } finally {
      setCreating(false);
    }
  }

  async function onDelete() {
    if (!deleteBucket) return;
    setDeleting(true);
    try {
      await api.deleteBucket(deleteBucket.name);
      toast({ title: "Bucket deleted", description: deleteBucket.name, variant: "success" });
      setDeleteBucket(null);
      await refresh();
    } catch (err) {
      toast({ title: "Delete failed", description: err instanceof Error ? err.message : String(err), variant: "destructive" });
    } finally {
      setDeleting(false);
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Buckets"
        description="S3-compatible buckets backed by zero-knowledge placement policies."
        actions={
          <Button onClick={() => { setCreateError(null); setPolicyRef(tenant?.placementDefaultPolicyRef ?? ""); setCreateOpen(true); }}>
            <Plus className="size-4" /> New bucket
          </Button>
        }
      />

      <Card>
        <CardContent className="p-0">
          {loading ? (
            <div className="space-y-2 p-4">
              {[0, 1, 2].map((i) => (
                <Skeleton key={i} className="h-10 w-full" />
              ))}
            </div>
          ) : error ? (
            <CardError message={error} onRetry={() => void refresh()} />
          ) : buckets.length === 0 ? (
            <EmptyState
              icon={Database}
              title="No buckets yet"
              description="Create your first bucket to start storing objects."
              action={<Button onClick={() => setCreateOpen(true)}><Plus className="size-4" /> New bucket</Button>}
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Name</TableHead>
                  <TableHead>Placement</TableHead>
                  <TableHead className="text-right">Objects</TableHead>
                  <TableHead className="text-right">Size</TableHead>
                  <TableHead className="w-[1%]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {buckets.map((b) => (
                  <TableRow key={b.name}>
                    <TableCell className="font-medium text-foreground">{b.name}</TableCell>
                    <TableCell>
                      <Badge variant="neutral">{b.placementPolicyRef || "default"}</Badge>
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{formatNumber(b.objectCount)}</TableCell>
                    <TableCell className="text-right tabular-nums">{formatBytes(b.bytesStored)}</TableCell>
                    <TableCell>
                      <div className="flex justify-end gap-1">
                        <Button variant="ghost" size="icon" aria-label="Configure" onClick={() => setConfigBucket(b)}>
                          <Settings2 className="size-4" />
                        </Button>
                        <Button variant="ghost" size="icon" aria-label="Delete" onClick={() => setDeleteBucket(b)}>
                          <Trash2 className="size-4 text-destructive" />
                        </Button>
                      </div>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* Create bucket */}
      <Dialog open={createOpen} onOpenChange={setCreateOpen}>
        <DialogContent>
          <form onSubmit={onCreate}>
            <DialogHeader>
              <DialogTitle>New bucket</DialogTitle>
              <DialogDescription>Buckets inherit a placement policy that governs backend mapping and erasure coding.</DialogDescription>
            </DialogHeader>
            <div className="space-y-4 py-4">
              <div className="space-y-1.5">
                <Label htmlFor="bucket-name">Bucket name</Label>
                <Input id="bucket-name" value={name} onChange={(e) => setName(e.target.value)} placeholder="my-bucket" required autoFocus />
              </div>
              <div className="space-y-1.5">
                <Label htmlFor="placement-ref">Placement policy</Label>
                <Input id="placement-ref" value={policyRef} onChange={(e) => setPolicyRef(e.target.value)} placeholder="default" required />
                <p className="text-xs text-muted-foreground">Defaults to your tenant policy. Manage policies on the Placement screen.</p>
              </div>
              {createError && <InlineError message={createError} />}
            </div>
            <DialogFooter>
              <DialogClose asChild>
                <Button type="button" variant="outline">Cancel</Button>
              </DialogClose>
              <Button type="submit" disabled={creating}>{creating ? "Creating…" : "Create bucket"}</Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>

      {/* Configure bucket */}
      <BucketConfigDialog bucket={configBucket} onClose={() => setConfigBucket(null)} />

      {/* Delete confirm */}
      <Dialog open={!!deleteBucket} onOpenChange={(o) => !o && setDeleteBucket(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Delete bucket</DialogTitle>
            <DialogDescription>
              Permanently delete <span className="font-medium text-foreground">{deleteBucket?.name}</span>? This cannot be undone.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">Cancel</Button>
            </DialogClose>
            <Button variant="destructive" onClick={onDelete} disabled={deleting}>{deleting ? "Deleting…" : "Delete"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

// BucketConfigDialog surfaces per-bucket configuration. Intra-tenant
// dedup is wired to the real GET/POST dedup-policy endpoint (which is
// operator-gated, so the control degrades to a read-only notice when
// the console session lacks access). Versioning / lifecycle / CORS /
// Object Lock are governed by the bucket's placement policy and are not
// yet self-service, so we say so honestly rather than rendering dead
// toggles.
function BucketConfigDialog({ bucket, onClose }: { bucket: Bucket | null; onClose: () => void }) {
  const { toast } = useToast();
  const [policy, setPolicy] = useState<DedupPolicy | null>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [gated, setGated] = useState(false);
  const [loadError, setLoadError] = useState<string | null>(null);

  useEffect(() => {
    if (!bucket) {
      setPolicy(null);
      setGated(false);
      setLoadError(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setLoadError(null);
    api
      .getDedupPolicy(bucket.name)
      .then((p) => {
        if (cancelled) return;
        if (p === null) {
          // null means the policy is gated for this session
          // (operator-managed); a genuine fault is thrown, not null.
          setGated(true);
        } else {
          setPolicy(p);
          setGated(false);
        }
      })
      .catch((e) => {
        // Network error or 5xx: surface it honestly instead of
        // leaving an unhandled rejection and a misleading interactive
        // toggle that does not reflect the real backend state.
        if (!cancelled) setLoadError(e instanceof Error ? e.message : String(e));
      })
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, [bucket]);

  async function toggleDedup(enabled: boolean) {
    if (!bucket) return;
    setSaving(true);
    try {
      const updated = await api.setDedupPolicy(bucket.name, { enabled, level: "object" });
      setPolicy(updated);
      toast({ title: enabled ? "Dedup enabled" : "Dedup disabled", description: bucket.name, variant: "success" });
    } catch (err) {
      toast({ title: "Could not update dedup", description: err instanceof Error ? err.message : String(err), variant: "destructive" });
    } finally {
      setSaving(false);
    }
  }

  return (
    <Dialog open={!!bucket} onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Configure {bucket?.name}</DialogTitle>
          <DialogDescription>Storage behaviour for this bucket.</DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-4">
          <div className="space-y-3 rounded-lg border border-border p-4">
            <div className="flex items-center justify-between gap-4">
              <div className="space-y-0.5">
                <div className="text-sm font-medium text-foreground">Intra-tenant dedup</div>
                <p className="text-xs text-muted-foreground">
                  Object-level deduplication within your tenant. Block-level dedup requires a Ceph RGW backend on a dedicated cell.
                </p>
              </div>
              {loading ? (
                <Skeleton className="h-6 w-11" />
              ) : loadError ? (
                <Badge variant="destructive">Unavailable</Badge>
              ) : gated ? (
                <Badge variant="neutral">Operator-managed</Badge>
              ) : (
                <Switch checked={!!policy?.enabled} disabled={saving} onCheckedChange={toggleDedup} aria-label="Toggle dedup" />
              )}
            </div>
            {loadError && <InlineError message={`Couldn't load dedup policy: ${loadError}`} />}
          </div>
          <div className="rounded-lg border border-border p-4">
            <div className="text-sm font-medium text-foreground">Versioning · Lifecycle · CORS · Object Lock</div>
            <p className="mt-1 text-xs text-muted-foreground">
              These are governed by the bucket's placement policy and are configured by operations. Adjust the placement policy on the
              Placement screen to change backend, erasure profile, and retention behaviour.
            </p>
          </div>
        </div>
        <DialogFooter>
          <DialogClose asChild>
            <Button type="button" variant="outline">Close</Button>
          </DialogClose>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
