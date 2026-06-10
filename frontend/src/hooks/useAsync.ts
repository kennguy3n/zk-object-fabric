import { useCallback, useEffect, useState } from "react";

export interface AsyncState<T> {
  // The last successful result, or null before the first one resolves.
  // Contract: a failed (re)load sets `error` but does NOT clear `data` —
  // the previous success is retained so a transient reload failure does
  // not blank the UI. Consumers must therefore branch in
  // loading -> error -> data priority order (never read `data` while
  // `error` is set if stale data would be misleading).
  data: T | null;
  loading: boolean;
  error: string | null;
  // reload re-runs the fetcher (e.g. after a mutation or a Retry click).
  reload: () => void;
}

function toMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

// useAsync runs an async fetcher on mount and whenever a dependency in
// `deps` changes, exposing the canonical loading / error / data triple
// plus a manual reload. It guards against setting state after unmount
// (or after deps change mid-flight) so a slow response can never
// clobber a newer one or warn about updating an unmounted component.
// On a failed (re)load it sets `error` but intentionally keeps the last
// successful `data` (see the AsyncState contract above) so callers can
// keep rendering prior results behind a retry affordance.
//
// Dependency contract: `fetcher` is deliberately excluded from the
// effect deps (callers pass a fresh inline closure every render, which
// would otherwise re-run the effect on every render). The effect
// instead re-runs on `deps` + an internal reload nonce, so callers MUST
// list every changing value their fetcher closes over in `deps` (e.g.
// `useAsync(() => api.get(id), [id])`). Fetchers that only close over
// the module-level `api` singleton — the case for all current callers —
// need no deps. The exhaustive-deps lint is suppressed for exactly this
// reason; it cannot see through the spread `[...deps, nonce]`.
export function useAsync<T>(
  fetcher: () => Promise<T>,
  deps: React.DependencyList = [],
): AsyncState<T> {
  const [data, setData] = useState<T | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [nonce, setNonce] = useState(0);

  const reload = useCallback(() => setNonce((n) => n + 1), []);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError(null);
    fetcher()
      .then((result) => {
        if (active) setData(result);
      })
      .catch((e) => {
        if (active) setError(toMessage(e));
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [...deps, nonce]);

  return { data, loading, error, reload };
}
