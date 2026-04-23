import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { ApiKey } from "../api/types";

export function ApiKeysPage() {
  const [keys, setKeys] = useState<ApiKey[]>([]);
  const [freshSecret, setFreshSecret] = useState<ApiKey | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
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

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>API keys</h1>
      <div className="panel row" style={{ justifyContent: "space-between" }}>
        <div className="muted" style={{ fontSize: 13 }}>
          API keys are S3-compatible credentials. Secret keys are shown once, at
          creation — store them in your secret manager.
        </div>
        <button
          onClick={async () => {
            setError(null);
            try {
              const k = await api.createApiKey();
              setFreshSecret(k);
              await refresh();
            } catch (err) {
              setError(err instanceof Error ? err.message : String(err));
            }
          }}
        >
          Create key
        </button>
      </div>
      {freshSecret?.secretKey && (
        <div className="panel" style={{ borderColor: "var(--accent)" }}>
          <div style={{ fontWeight: 600 }}>New key — copy this now</div>
          <div className="muted" style={{ fontSize: 13, marginBottom: 8 }}>
            The secret will not be shown again.
          </div>
          <Code label="Access key" value={freshSecret.accessKey} />
          <Code label="Secret key" value={freshSecret.secretKey} />
          <button className="secondary" onClick={() => setFreshSecret(null)}>
            Dismiss
          </button>
        </div>
      )}
      {error && <div className="panel danger-text">{error}</div>}
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Access key</th>
              <th>Created</th>
              <th>Last used</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={4} className="muted">
                  Loading…
                </td>
              </tr>
            )}
            {!loading && keys.length === 0 && (
              <tr>
                <td colSpan={4} className="muted">
                  No keys yet.
                </td>
              </tr>
            )}
            {keys.map((k) => (
              <tr key={k.accessKey}>
                <td style={{ fontFamily: "monospace" }}>{k.accessKey}</td>
                <td>{formatTimestamp(k.createdAt)}</td>
                <td>{k.lastUsedAt ? formatTimestamp(k.lastUsedAt) : "never"}</td>
                <td style={{ textAlign: "right" }}>
                  <button
                    className="danger"
                    onClick={async () => {
                      if (!confirm("Revoke this key? Clients using it will start getting 403s.")) {
                        return;
                      }
                      try {
                        await api.revokeApiKey(k.accessKey);
                        await refresh();
                      } catch (err) {
                        setError(err instanceof Error ? err.message : String(err));
                      }
                    }}
                  >
                    Revoke
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// formatTimestamp renders an ISO timestamp as a locale string, but
// collapses Go's zero time (0001-01-01T00:00:00Z) and any other
// unparseable / pre-epoch value to "unknown". The list endpoint
// returns zero timestamps when the binding schema predates the
// createdAt column; rendering those as "1/1/1" in the UI was
// confusing operators into thinking keys were decades old.
function formatTimestamp(value: string | undefined): string {
  if (!value) {
    return "unknown";
  }
  const d = new Date(value);
  if (Number.isNaN(d.getTime()) || d.getUTCFullYear() <= 1) {
    return "unknown";
  }
  return d.toLocaleString();
}

function Code({ label, value }: { label: string; value: string }) {
  return (
    <div style={{ marginBottom: 8 }}>
      <div className="muted" style={{ fontSize: 12 }}>
        {label}
      </div>
      <code
        style={{
          display: "block",
          fontFamily: "monospace",
          padding: "6px 8px",
          borderRadius: 4,
          background: "rgba(92, 200, 255, 0.08)",
          border: "1px solid var(--border)",
          wordBreak: "break-all",
        }}
      >
        {value}
      </code>
    </div>
  );
}
