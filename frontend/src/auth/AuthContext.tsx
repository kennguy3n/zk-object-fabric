import { createContext, useCallback, useContext, useEffect, useMemo, useState } from "react";

import { api } from "../api/client";
import type { Tenant } from "../api/types";

// AuthContext exposes the current tenant identity + token to the
// rest of the SPA. The token is persisted to sessionStorage so a
// page reload doesn't kick the operator back to /login. We
// deliberately avoid localStorage because the token must be
// discarded when the tab closes.
interface AuthState {
  tenant: Tenant | null;
  token: string | null;
}

interface AuthContextValue extends AuthState {
  signIn(email: string, password: string): Promise<void>;
  signUp(input: {
    email: string;
    password: string;
    tenantName: string;
    captchaToken?: string;
  }): Promise<void>;
  signOut(): void;
}

const AuthContext = createContext<AuthContextValue | null>(null);

const STORAGE_KEY = "zk-fabric.auth";

function readPersisted(): AuthState {
  try {
    const raw = sessionStorage.getItem(STORAGE_KEY);
    if (!raw) return { tenant: null, token: null };
    return JSON.parse(raw) as AuthState;
  } catch {
    return { tenant: null, token: null };
  }
}

// applyToClient configures the shared ApiClient with the current
// token + tenant scope. React's commit phase runs child effects
// before parent effects, so configuring the client purely inside
// the AuthProvider's useEffect loses the race with mounting
// dashboard pages that issue tenant-scoped calls in their own
// mount effects. We therefore apply it synchronously: at module
// load (from persisted sessionStorage), inside signIn/signUp
// right after the server replies, and on signOut.
function applyToClient(state: AuthState) {
  api.setToken(state.token ?? undefined);
  api.setTenantScope(state.tenant?.id);
}

// Seed the shared ApiClient before React renders so the very first
// component-mount effect already sees a scoped client if the user
// has a persisted session. Executed at module load because the
// ApiClient itself is a module-level singleton.
applyToClient(readPersisted());

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [state, setState] = useState<AuthState>(() => readPersisted());

  useEffect(() => {
    if (state.token && state.tenant) {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify(state));
    } else {
      sessionStorage.removeItem(STORAGE_KEY);
    }
  }, [state]);

  const signIn = useCallback(async (email: string, password: string) => {
    const { tenant, token } = await api.login(email, password);
    applyToClient({ tenant, token });
    setState({ tenant, token });
  }, []);

  const signUp = useCallback(
    async (input: {
      email: string;
      password: string;
      tenantName: string;
      captchaToken?: string;
    }) => {
      const { tenant, token } = await api.signup(input);
      applyToClient({ tenant, token });
      setState({ tenant, token });
    },
    [],
  );

  const signOut = useCallback(() => {
    applyToClient({ tenant: null, token: null });
    setState({ tenant: null, token: null });
  }, []);

  const value = useMemo<AuthContextValue>(
    () => ({ ...state, signIn, signUp, signOut }),
    [state, signIn, signUp, signOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthContextValue {
  const ctx = useContext(AuthContext);
  if (!ctx) {
    throw new Error("useAuth must be used inside <AuthProvider>");
  }
  return ctx;
}
