import { Navigate, NavLink, Route, Routes, useLocation } from "react-router-dom";

import { AuthProvider, useAuth } from "./auth/AuthContext";
import { LoginPage } from "./pages/LoginPage";
import { SignupPage } from "./pages/SignupPage";
import { DashboardPage } from "./pages/DashboardPage";
import { BucketsPage } from "./pages/BucketsPage";
import { ApiKeysPage } from "./pages/ApiKeysPage";
import { PlacementPolicyPage } from "./pages/PlacementPolicyPage";
import { B2BPage } from "./pages/B2BPage";

export function App() {
  return (
    <AuthProvider>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/signup" element={<SignupPage />} />
        <Route
          path="/*"
          element={
            <RequireAuth>
              <ConsoleShell />
            </RequireAuth>
          }
        />
      </Routes>
    </AuthProvider>
  );
}

function RequireAuth({ children }: { children: React.ReactNode }) {
  const { tenant } = useAuth();
  const location = useLocation();
  if (!tenant) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }
  return <>{children}</>;
}

function ConsoleShell() {
  const { tenant, signOut } = useAuth();
  const isB2B =
    tenant?.contractType === "b2b_dedicated" ||
    tenant?.contractType === "sovereign";

  return (
    <div style={{ display: "grid", gridTemplateColumns: "240px 1fr", minHeight: "100vh" }}>
      <aside
        style={{
          background: "var(--panel)",
          borderRight: "1px solid var(--border)",
          padding: 20,
        }}
      >
        <div style={{ marginBottom: 24 }}>
          <div style={{ fontWeight: 700 }}>ZK Object Fabric</div>
          <div className="muted" style={{ fontSize: 12 }}>
            {tenant?.name} · <span className="badge accent">{tenant?.contractType}</span>
          </div>
        </div>
        <nav className="stack">
          <SideLink to="/">Dashboard</SideLink>
          <SideLink to="/buckets">Buckets</SideLink>
          <SideLink to="/api-keys">API Keys</SideLink>
          <SideLink to="/placement">Placement Policy</SideLink>
          {isB2B && <SideLink to="/b2b">Dedicated Cells</SideLink>}
        </nav>
        <div style={{ marginTop: 32 }}>
          <button className="secondary" onClick={signOut}>
            Sign out
          </button>
        </div>
      </aside>
      <main style={{ padding: 32 }}>
        <Routes>
          <Route index element={<DashboardPage />} />
          <Route path="buckets" element={<BucketsPage />} />
          <Route path="api-keys" element={<ApiKeysPage />} />
          <Route path="placement" element={<PlacementPolicyPage />} />
          {isB2B && <Route path="b2b" element={<B2BPage />} />}
          <Route path="*" element={<Navigate to="/" replace />} />
        </Routes>
      </main>
    </div>
  );
}

function SideLink({ to, children }: { to: string; children: React.ReactNode }) {
  return (
    <NavLink
      to={to}
      end={to === "/"}
      style={({ isActive }) => ({
        color: isActive ? "var(--accent)" : "var(--text)",
        padding: "6px 8px",
        borderRadius: 6,
        background: isActive ? "rgba(92, 200, 255, 0.08)" : "transparent",
      })}
    >
      {children}
    </NavLink>
  );
}
