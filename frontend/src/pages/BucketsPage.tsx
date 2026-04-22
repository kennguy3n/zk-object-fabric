import { useCallback, useEffect, useState } from "react";

import { api } from "../api/client";
import type { Bucket } from "../api/types";
import { useAuth } from "../auth/AuthContext";
import { formatBytes } from "../format";

export function BucketsPage() {
  const { tenant } = useAuth();
  const [buckets, setBuckets] = useState<Bucket[]>([]);
  const [name, setName] = useState("");
  const [policyRef, setPolicyRef] = useState(tenant?.placementDefaultPolicyRef ?? "");
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
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

  return (
    <div className="stack">
      <h1 style={{ margin: 0 }}>Buckets</h1>
      <form
        className="panel row"
        style={{ gap: 12 }}
        onSubmit={async (e) => {
          e.preventDefault();
          setError(null);
          try {
            await api.createBucket(name.trim(), policyRef.trim());
            setName("");
            await refresh();
          } catch (err) {
            setError(err instanceof Error ? err.message : String(err));
          }
        }}
      >
        <div style={{ flex: 2 }}>
          <label htmlFor="bucket-name">Bucket name</label>
          <input
            id="bucket-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            required
          />
        </div>
        <div style={{ flex: 2 }}>
          <label htmlFor="placement-ref">Placement policy</label>
          <input
            id="placement-ref"
            value={policyRef}
            onChange={(e) => setPolicyRef(e.target.value)}
            required
          />
        </div>
        <div style={{ alignSelf: "end" }}>
          <button type="submit">Create bucket</button>
        </div>
      </form>
      {error && <div className="panel danger-text">{error}</div>}
      <div className="panel" style={{ padding: 0 }}>
        <table>
          <thead>
            <tr>
              <th>Name</th>
              <th>Placement</th>
              <th>Objects</th>
              <th>Size</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            {loading && (
              <tr>
                <td colSpan={5} className="muted">
                  Loading…
                </td>
              </tr>
            )}
            {!loading && buckets.length === 0 && (
              <tr>
                <td colSpan={5} className="muted">
                  No buckets yet. Create one above.
                </td>
              </tr>
            )}
            {buckets.map((b) => (
              <tr key={b.name}>
                <td>{b.name}</td>
                <td>
                  <span className="badge">{b.placementPolicyRef}</span>
                </td>
                <td>{b.objectCount.toLocaleString()}</td>
                <td>{formatBytes(b.bytesStored)}</td>
                <td style={{ textAlign: "right" }}>
                  <button
                    className="danger"
                    onClick={async () => {
                      if (!confirm(`Delete bucket ${b.name}? This cannot be undone.`)) {
                        return;
                      }
                      try {
                        await api.deleteBucket(b.name);
                        await refresh();
                      } catch (err) {
                        setError(err instanceof Error ? err.message : String(err));
                      }
                    }}
                  >
                    Delete
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
