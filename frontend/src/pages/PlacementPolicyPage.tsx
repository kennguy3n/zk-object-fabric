import { useEffect, useMemo, useState } from "react";

import { api } from "../api/client";
import type { PlacementPolicy } from "../api/types";

// PlacementPolicyPage is a minimal YAML + structured editor for the
// placement policy schema documented in docs/PROPOSAL.md §3.6.
// The structured form surfaces the most common knobs (allowed
// countries, replication factor, cache preference) as native form
// controls while keeping the canonical YAML editable for power
// users. The YAML is always the source of truth — the structured
// summary is derived.
export function PlacementPolicyPage() {
  const [policies, setPolicies] = useState<PlacementPolicy[]>([]);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [yaml, setYaml] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    api
      .listPlacementPolicies()
      .then((p) => {
        if (cancelled) return;
        setPolicies(p);
        if (p.length > 0) {
          setSelectedId(p[0].id);
          setYaml(p[0].yaml);
        }
      })
      .catch((e) => !cancelled && setError(e instanceof Error ? e.message : String(e)))
      .finally(() => !cancelled && setLoading(false));
    return () => {
      cancelled = true;
    };
  }, []);

  const selected = useMemo(
    () => policies.find((p) => p.id === selectedId) ?? null,
    [policies, selectedId],
  );

  const summary = useMemo(() => summarizeYaml(yaml), [yaml]);

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Placement policy</h1>
      <div className="muted" style={{ fontSize: 13 }}>
        Policies are evaluated by the gateway before each PUT to decide which
        providers and regions are allowed. See docs/PROPOSAL.md §3.6.
      </div>
      <div className="grid" style={{ gridTemplateColumns: "240px 1fr", gap: 16 }}>
        <div className="panel stack">
          {loading && <div className="muted">Loading…</div>}
          {!loading && policies.length === 0 && (
            <div className="muted">No policies yet.</div>
          )}
          {policies.map((p) => (
            <button
              key={p.id}
              className={p.id === selectedId ? "" : "secondary"}
              style={{ textAlign: "left" }}
              onClick={() => {
                setSelectedId(p.id);
                setYaml(p.yaml);
              }}
            >
              {p.name}
            </button>
          ))}
        </div>
        <div className="stack">
          <div className="panel">
            <div className="muted" style={{ fontSize: 12, marginBottom: 4 }}>
              Summary
            </div>
            <div style={{ fontSize: 13 }}>
              Allowed countries: <b>{summary.countries.join(", ") || "—"}</b>
              <br />
              Replication factor: <b>{summary.replication ?? "—"}</b>
              <br />
              Cache preference: <b>{summary.cache ?? "—"}</b>
            </div>
          </div>
          <div className="panel">
            <label htmlFor="yaml">YAML</label>
            <textarea
              id="yaml"
              value={yaml}
              onChange={(e) => setYaml(e.target.value)}
              rows={20}
              style={{ fontFamily: "monospace", fontSize: 13 }}
            />
            {error && <div className="danger-text">{error}</div>}
            <div className="row" style={{ marginTop: 12, gap: 8 }}>
              <button
                disabled={!selected || saving}
                onClick={async () => {
                  if (!selected) return;
                  setSaving(true);
                  setError(null);
                  try {
                    const updated = await api.savePlacementPolicy({
                      id: selected.id,
                      name: selected.name,
                      yaml,
                    });
                    setPolicies((ps) => ps.map((p) => (p.id === updated.id ? updated : p)));
                  } catch (err) {
                    setError(err instanceof Error ? err.message : String(err));
                  } finally {
                    setSaving(false);
                  }
                }}
              >
                {saving ? "Saving…" : "Save"}
              </button>
              <button
                className="secondary"
                disabled={!selected}
                onClick={() => selected && setYaml(selected.yaml)}
              >
                Discard changes
              </button>
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

interface PolicySummary {
  countries: string[];
  replication: number | null;
  cache: string | null;
}

// summarizeYaml is a regex-based extractor for the three headline
// knobs. We deliberately avoid a full YAML parser in the scaffold;
// the gateway is the source of truth and will reject invalid YAML on
// save. Replace with `js-yaml` once the schema stabilizes.
export function summarizeYaml(yaml: string): PolicySummary {
  const countries: string[] = [];
  const countryLine = yaml.match(/allowed_countries:\s*\[([^\]]*)\]/);
  if (countryLine) {
    for (const raw of countryLine[1].split(",")) {
      const v = raw.trim().replace(/^['"]|['"]$/g, "");
      if (v) countries.push(v);
    }
  }
  const repl = yaml.match(/replication_factor:\s*(\d+)/);
  const cache = yaml.match(/cache:\s*([\w-]+)/);
  return {
    countries,
    replication: repl ? Number(repl[1]) : null,
    cache: cache ? cache[1] : null,
  };
}
