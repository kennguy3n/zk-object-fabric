import { Copy, KeyRound, Plus } from "lucide-react";
import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { ApiKey } from "../api/types";
import { PageHeader } from "../components/PageHeader";
import { CardError, EmptyState } from "../components/states";
import { formatDateTime, formatRelative } from "../format";
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
import { Skeleton } from "../ui/skeleton";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "../ui/table";
import { useToast } from "../ui/toast";

export function ApiKeysPage() {
  const { toast } = useToast();
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [freshSecret, setFreshSecret] = useState<ApiKey | null>(null);
  const [revoking, setRevoking] = useState<ApiKey | null>(null);
  const [revokeBusy, setRevokeBusy] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setKeys(await api.listApiKeys());
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  async function onCreate() {
    setCreating(true);
    try {
      const k = await api.createApiKey();
      setFreshSecret(k);
      await refresh();
    } catch (err) {
      toast({ title: "Could not create key", description: err instanceof Error ? err.message : String(err), variant: "destructive" });
    } finally {
      setCreating(false);
    }
  }

  async function onRevoke() {
    if (!revoking) return;
    setRevokeBusy(true);
    try {
      await api.revokeApiKey(revoking.accessKey);
      toast({ title: "Key revoked", description: revoking.accessKey, variant: "success" });
      setRevoking(null);
      await refresh();
    } catch (err) {
      toast({ title: "Revoke failed", description: err instanceof Error ? err.message : String(err), variant: "destructive" });
    } finally {
      setRevokeBusy(false);
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="API keys"
        description="S3-compatible credentials. Secret keys are shown once, at creation — store them in your secret manager."
        actions={
          <Button onClick={onCreate} disabled={creating}>
            <Plus className="size-4" /> {creating ? "Creating…" : "Create key"}
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
          ) : keys.length === 0 ? (
            <EmptyState
              icon={KeyRound}
              title="No API keys"
              description="Create a key to access your buckets over the S3 API."
              action={<Button onClick={onCreate} disabled={creating}><Plus className="size-4" /> Create key</Button>}
            />
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Access key</TableHead>
                  <TableHead>Created</TableHead>
                  <TableHead>Last used</TableHead>
                  <TableHead className="w-[1%]" />
                </TableRow>
              </TableHeader>
              <TableBody>
                {keys.map((k) => (
                  <TableRow key={k.accessKey}>
                    <TableCell className="font-mono text-xs">{k.accessKey}</TableCell>
                    <TableCell className="text-muted-foreground">{formatDateTime(k.createdAt)}</TableCell>
                    <TableCell className="text-muted-foreground">{k.lastUsedAt ? formatRelative(k.lastUsedAt) : <Badge variant="neutral">never</Badge>}</TableCell>
                    <TableCell className="text-right">
                      <Button variant="outline" size="sm" onClick={() => setRevoking(k)}>Revoke</Button>
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      {/* New secret reveal */}
      <Dialog open={!!freshSecret} onOpenChange={(o) => !o && setFreshSecret(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>New API key</DialogTitle>
            <DialogDescription>Copy the secret key now — it will not be shown again.</DialogDescription>
          </DialogHeader>
          <div className="space-y-3 py-4">
            <SecretField label="Access key" value={freshSecret?.accessKey ?? ""} />
            <SecretField label="Secret key" value={freshSecret?.secretKey ?? ""} />
          </div>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button">Done</Button>
            </DialogClose>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      {/* Revoke confirm */}
      <Dialog open={!!revoking} onOpenChange={(o) => !o && setRevoking(null)}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke key</DialogTitle>
            <DialogDescription>
              Revoke <span className="font-mono text-xs">{revoking?.accessKey}</span>? Clients using it will immediately start getting 403s.
            </DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">Cancel</Button>
            </DialogClose>
            <Button variant="destructive" onClick={onRevoke} disabled={revokeBusy}>{revokeBusy ? "Revoking…" : "Revoke"}</Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function SecretField({ label, value }: { label: string; value: string }) {
  const { toast } = useToast();
  async function copy() {
    try {
      await navigator.clipboard.writeText(value);
      toast({ title: "Copied", description: label, variant: "success" });
    } catch {
      toast({ title: "Copy failed", description: "Select the value and copy manually.", variant: "destructive" });
    }
  }
  return (
    <div className="space-y-1">
      <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</div>
      <div className="flex items-center gap-2">
        <code className="flex-1 overflow-x-auto rounded-md border border-border bg-muted px-3 py-2 font-mono text-xs text-foreground">{value}</code>
        <Button variant="outline" size="icon" aria-label={`Copy ${label}`} onClick={copy}>
          <Copy className="size-4" />
        </Button>
      </div>
    </div>
  );
}
