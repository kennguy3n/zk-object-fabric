import { Globe, Layers, MapPin } from "lucide-react";
import { useEffect, useMemo, useState } from "react";

import { api } from "../api/client";
import { PageHeader } from "../components/PageHeader";
import { CardError, EmptyState, InlineError } from "../components/states";
import { useAsync } from "../hooks/useAsync";
import { cn } from "../lib/cn";
import { Badge } from "../ui/badge";
import { Button } from "../ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Label } from "../ui/label";
import { Skeleton } from "../ui/skeleton";
import { Textarea } from "../ui/input";
import { useToast } from "../ui/toast";

// PlacementPolicyPage is a structured + raw editor for the placement
// policy schema documented in docs/PROPOSAL.md §3.6. The structured
// summary (allowed countries, provider fan-out, cache preference) is
// derived from the canonical buffer, which stays the source of truth.
export function PlacementPolicyPage() {
  const { toast } = useToast();
  const { data, loading, error, reload } = useAsync(() => api.listPlacementPolicies(), []);
  const policies = useMemo(() => data ?? [], [data]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [yaml, setYaml] = useState("");
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  // Seed the selection and editor buffer from the first successful
  // load (and re-seed after a save-triggered reload). The functional
  // updaters keep any in-progress edit: yaml is only replaced while it
  // is still empty, so a reload never clobbers unsaved keystrokes.
  useEffect(() => {
    if (policies.length === 0) return;
    setSelectedId((cur) => cur ?? policies[0].id);
    setYaml((cur) => (cur === "" ? policies[0].yaml : cur));
  }, [policies]);

  const selected = useMemo(
    () => policies.find((p) => p.id === selectedId) ?? null,
    [policies, selectedId],
  );
  const summary = useMemo(() => summarizeYaml(yaml), [yaml]);
  const dirty = !!selected && selected.yaml !== yaml;

  async function onSave() {
    if (!selected) return;
    setSaving(true);
    setSaveError(null);
    try {
      const saved = await api.savePlacementPolicy({ id: selected.id, name: selected.name, yaml });
      toast({ title: "Policy saved", description: selected.name, variant: "success" });
      // Adopt the backend's canonical round-trip of the document as the
      // editor buffer. The user's input may differ only in formatting
      // (key order, whitespace), so comparing their raw text against the
      // server form would otherwise leave a misleading "Unsaved changes"
      // badge. Seeding from `saved.yaml` — the exact shape a subsequent
      // listPlacementPolicies() returns — makes `dirty` resolve to false
      // deterministically regardless of how the user typed the JSON.
      setYaml(saved.yaml);
      // Re-fetch so the list reflects the persisted document as the
      // single source of truth; the seed effect leaves the buffer (now
      // the canonical form) untouched.
      reload();
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : String(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <div className="space-y-6">
      <PageHeader
        title="Placement policies"
        description="Evaluated by the gateway before each PUT to decide which providers and regions are allowed. See docs/PROPOSAL.md §3.6."
      />

      <div className="grid gap-6 lg:grid-cols-[260px_1fr]">
        <Card>
          <CardHeader>
            <CardTitle>Policies</CardTitle>
          </CardHeader>
          <CardContent className="space-y-1">
            {/* Skeleton only covers the first load. useAsync keeps the
                last list across a save-triggered reload, so gating on
                an empty list avoids a sidebar flicker after every save. */}
            {loading && policies.length === 0 ? (
              <div className="space-y-2">
                {[0, 1, 2].map((i) => <Skeleton key={i} className="h-9 w-full" />)}
              </div>
            ) : error ? (
              <CardError message={error} onRetry={reload} />
            ) : policies.length === 0 ? (
              <EmptyState icon={Layers} title="No policies" description="Placement policies are provisioned with your tenant." />
            ) : (
              policies.map((p) => (
                <button
                  key={p.id}
                  onClick={() => { setSelectedId(p.id); setYaml(p.yaml); setSaveError(null); }}
                  className={cn(
                    "w-full rounded-md px-3 py-2 text-left text-sm transition-colors",
                    p.id === selectedId ? "bg-primary/10 font-medium text-primary" : "text-foreground hover:bg-muted",
                  )}
                >
                  {p.name}
                </button>
              ))
            )}
          </CardContent>
        </Card>

        <div className="space-y-6">
          <div className="grid gap-4 sm:grid-cols-3">
            <SummaryStat icon={Globe} label="Allowed countries" value={summary.countries.join(", ") || "—"} />
            <SummaryStat icon={Layers} label="Provider fan-out" value={summary.replication != null ? `${summary.replication}×` : "—"} />
            <SummaryStat icon={MapPin} label="Cache preference" value={summary.cache ?? "—"} />
          </div>

          <Card>
            <CardHeader className="flex-row items-center justify-between">
              <CardTitle>{selected ? selected.name : "Policy"} — definition</CardTitle>
              {dirty && <Badge variant="warning">Unsaved changes</Badge>}
            </CardHeader>
            <CardContent className="space-y-3">
              <Label htmlFor="policy-yaml">Canonical policy (JSON / YAML)</Label>
              <Textarea
                id="policy-yaml"
                value={yaml}
                onChange={(e) => setYaml(e.target.value)}
                rows={18}
                spellCheck={false}
                className="font-mono text-xs"
                disabled={!selected}
              />
              {saveError && <InlineError message={saveError} />}
              <div className="flex gap-2">
                <Button disabled={!selected || saving || !dirty} onClick={onSave}>
                  {saving ? "Saving…" : "Save policy"}
                </Button>
                <Button variant="outline" disabled={!selected || !dirty} onClick={() => selected && setYaml(selected.yaml)}>
                  Discard changes
                </Button>
              </div>
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  );
}

function SummaryStat({ icon: Icon, label, value }: { icon: typeof Globe; label: string; value: string }) {
  return (
    <Card>
      <CardContent className="flex items-center gap-3 p-4">
        <span className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/15 text-primary">
          <Icon className="size-5" />
        </span>
        <div className="min-w-0">
          <div className="text-xs font-medium uppercase tracking-wide text-muted-foreground">{label}</div>
          <div className="truncate text-sm font-medium text-foreground">{value}</div>
        </div>
      </CardContent>
    </Card>
  );
}

interface PolicySummary {
  countries: string[];
  replication: number | null;
  cache: string | null;
}

// summarizeYaml extracts the three headline knobs from the editor
// buffer. backendToFrontendPolicy round-trips placement policies as
// canonical JSON (the gateway accepts both YAML and JSON), mirroring
// placement_policy.Policy in metadata/placement_policy/policy.go:
//
//   { tenant, bucket, policy: { encryption, placement: {
//       provider, region, country, storage_class, cache_location } } }
//
// Replication factor is not an exposed knob, so the summary reports the
// provider fan-out as a proxy. A raw-YAML paste falls back to the
// legacy regex extractor so the summary degrades gracefully.
export function summarizeYaml(yaml: string): PolicySummary {
  try {
    const parsed = JSON.parse(yaml) as {
      policy?: {
        placement?: {
          country?: unknown;
          provider?: unknown;
          cache_location?: unknown;
        };
      };
    };
    const placement = parsed.policy?.placement ?? {};
    const countries = Array.isArray(placement.country)
      ? placement.country.filter((c): c is string => typeof c === "string")
      : [];
    const providers = Array.isArray(placement.provider)
      ? placement.provider.filter((p): p is string => typeof p === "string")
      : [];
    const cache =
      typeof placement.cache_location === "string" && placement.cache_location !== ""
        ? placement.cache_location
        : null;
    return {
      countries,
      replication: providers.length > 0 ? providers.length : null,
      cache,
    };
  } catch {
    return summarizeYamlLegacy(yaml);
  }
}

// summarizeYamlLegacy is the pre-JSON regex extractor, preserved so the
// summary panel still renders something useful when an operator
// hand-edits the textarea into YAML while the schema is still in flux.
function summarizeYamlLegacy(yaml: string): PolicySummary {
  const countries: string[] = [];
  const countryLine = yaml.match(/(?:allowed_countries|country):\s*\[([^\]]*)\]/);
  if (countryLine) {
    for (const raw of countryLine[1].split(",")) {
      const v = raw.trim().replace(/^['"]|['"]$/g, "");
      if (v) countries.push(v);
    }
  }
  const repl = yaml.match(/replication_factor:\s*(\d+)/);
  const cache = yaml.match(/cache(?:_location)?:\s*([\w-]+)/);
  return {
    countries,
    replication: repl ? Number(repl[1]) : null,
    cache: cache ? cache[1] : null,
  };
}
