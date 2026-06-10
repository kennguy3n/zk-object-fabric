import {
  Boxes,
  Building2,
  Coins,
  Gauge,
  HardDrive,
  KeyRound,
  LayoutDashboard,
  type LucideIcon,
  Map as MapIcon,
  Replace,
  Server,
  ShieldCheck,
} from "lucide-react";
import { Suspense, lazy, useState } from "react";
import { Navigate, NavLink, Route, Routes, useLocation } from "react-router-dom";

import { AuthProvider, useAuth } from "./auth/AuthContext";
import { cn } from "./lib/cn";
import { Badge } from "./ui/badge";
import { Button } from "./ui/button";
import { Skeleton } from "./ui/skeleton";

// Auth pages are the cold-start entry point, so they stay in the main
// bundle to avoid a Suspense flash before the tenant has a session.
import { LoginPage } from "./pages/LoginPage";
import { SignupPage } from "./pages/SignupPage";

// Authenticated console pages are code-split: each route loads on
// demand, keeping the initial bundle to the shell + auth flow. The
// chart-heavy pages (Dashboard, Billing, Wasabi Health) pull the
// shared recharts vendor chunk only when first visited.
const ApiKeysPage = lazy(() => import("./pages/ApiKeysPage").then((m) => ({ default: m.ApiKeysPage })));
const AccountPage = lazy(() => import("./pages/AccountPage").then((m) => ({ default: m.AccountPage })));
const BillingPage = lazy(() => import("./pages/BillingPage").then((m) => ({ default: m.BillingPage })));
const BucketsPage = lazy(() => import("./pages/BucketsPage").then((m) => ({ default: m.BucketsPage })));
const CellsPage = lazy(() => import("./pages/CellsPage").then((m) => ({ default: m.CellsPage })));
const DashboardPage = lazy(() => import("./pages/DashboardPage").then((m) => ({ default: m.DashboardPage })));
const MigrationsPage = lazy(() => import("./pages/MigrationsPage").then((m) => ({ default: m.MigrationsPage })));
const OperationsPage = lazy(() => import("./pages/OperationsPage").then((m) => ({ default: m.OperationsPage })));
const PlacementPolicyPage = lazy(() => import("./pages/PlacementPolicyPage").then((m) => ({ default: m.PlacementPolicyPage })));
const WasabiHealthPage = lazy(() => import("./pages/WasabiHealthPage").then((m) => ({ default: m.WasabiHealthPage })));

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

interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
}

interface NavGroup {
  heading: string;
  items: NavItem[];
}

// isB2B distinguishes dedicated-cell contracts, which unlock the
// Dedicated Cells screen. The same predicate gates the route below.
function isDedicatedContract(contractType?: string): boolean {
  return contractType === "b2b_dedicated" || contractType === "sovereign";
}

function navGroups(isB2B: boolean): NavGroup[] {
  const groups: NavGroup[] = [
    {
      heading: "Monitor",
      items: [
        { to: "/", label: "Dashboard", icon: LayoutDashboard },
        { to: "/operations", label: "Operations", icon: Gauge },
        { to: "/wasabi-health", label: "Wasabi Health", icon: ShieldCheck },
      ],
    },
    {
      heading: "Storage",
      items: [
        { to: "/buckets", label: "Buckets", icon: HardDrive },
        { to: "/placement", label: "Placement Policies", icon: MapIcon },
        { to: "/migrations", label: "Migrations", icon: Replace },
      ],
    },
    {
      heading: "Account",
      items: [
        { to: "/billing", label: "Billing", icon: Coins },
        { to: "/api-keys", label: "API Keys", icon: KeyRound },
        { to: "/account", label: "Tenant", icon: Building2 },
      ],
    },
  ];
  if (isB2B) {
    groups.push({
      heading: "Dedicated",
      items: [{ to: "/cells", label: "Dedicated Cells", icon: Server }],
    });
  }
  return groups;
}

function ConsoleShell() {
  const { tenant, signOut } = useAuth();
  const isB2B = isDedicatedContract(tenant?.contractType);
  const groups = navGroups(isB2B);

  return (
    <div className="min-h-screen bg-background lg:grid lg:grid-cols-[256px_1fr]">
      <Sidebar groups={groups} tenantName={tenant?.name} contractType={tenant?.contractType} onSignOut={signOut} />
      <main className="min-w-0 px-5 py-8 sm:px-8">
        <div className="mx-auto w-full max-w-6xl">
          <Suspense fallback={<RouteFallback />}>
            <Routes>
              <Route index element={<DashboardPage />} />
              <Route path="buckets" element={<BucketsPage />} />
              <Route path="placement" element={<PlacementPolicyPage />} />
              <Route path="migrations" element={<MigrationsPage />} />
              <Route path="billing" element={<BillingPage />} />
              <Route path="api-keys" element={<ApiKeysPage />} />
              <Route path="account" element={<AccountPage />} />
              <Route path="operations" element={<OperationsPage />} />
              <Route path="wasabi-health" element={<WasabiHealthPage />} />
              {isB2B && <Route path="cells" element={<CellsPage />} />}
              {/* Backwards-compatible redirects for the pre-redesign paths. */}
              <Route path="cost" element={<Navigate to="/billing" replace />} />
              <Route path="b2b" element={<Navigate to={isB2B ? "/cells" : "/"} replace />} />
              <Route path="*" element={<Navigate to="/" replace />} />
            </Routes>
          </Suspense>
        </div>
      </main>
    </div>
  );
}

// RouteFallback is shown while a lazily-loaded page chunk is in
// flight. It mirrors the page-header + content skeleton the pages
// render during their own data fetch, so the transition between the
// chunk download and the page's loading state is visually seamless.
function RouteFallback() {
  return (
    <div className="space-y-6">
      <div className="space-y-2">
        <Skeleton className="h-7 w-48" />
        <Skeleton className="h-4 w-80" />
      </div>
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {[0, 1, 2, 3].map((i) => (
          <Skeleton key={i} className="h-28 w-full" />
        ))}
      </div>
      <Skeleton className="h-64 w-full" />
    </div>
  );
}

function Sidebar({
  groups,
  tenantName,
  contractType,
  onSignOut,
}: {
  groups: NavGroup[];
  tenantName?: string;
  contractType?: string;
  onSignOut: () => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <>
      {/* Mobile top bar with a menu toggle. */}
      <div className="flex items-center justify-between border-b border-border bg-card px-4 py-3 lg:hidden">
        <BrandMark />
        <Button variant="outline" size="sm" onClick={() => setOpen((v) => !v)}>
          {open ? "Close" : "Menu"}
        </Button>
      </div>
      <aside
        className={cn(
          "flex flex-col gap-6 border-r border-border bg-card px-4 py-6 lg:sticky lg:top-0 lg:h-screen",
          open ? "block" : "hidden lg:flex",
        )}
      >
        <div className="hidden lg:block">
          <BrandMark />
        </div>
        <div className="rounded-lg border border-border bg-background/60 p-3">
          <div className="truncate text-sm font-medium text-foreground">
            {tenantName ?? "—"}
          </div>
          {contractType && (
            <Badge variant="neutral" className="mt-1.5">
              {contractType}
            </Badge>
          )}
        </div>
        <nav className="flex flex-1 flex-col gap-5 overflow-y-auto">
          {groups.map((group) => (
            <div key={group.heading} className="space-y-1">
              <div className="px-2 text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
                {group.heading}
              </div>
              {group.items.map((item) => (
                <SideLink key={item.to} item={item} onNavigate={() => setOpen(false)} />
              ))}
            </div>
          ))}
        </nav>
        <Button variant="outline" size="sm" onClick={onSignOut} className="justify-start">
          Sign out
        </Button>
      </aside>
    </>
  );
}

function BrandMark() {
  return (
    <div className="flex items-center gap-2">
      <span className="flex size-8 items-center justify-center rounded-lg bg-primary/15 text-primary">
        <Boxes className="size-5" />
      </span>
      <div className="leading-tight">
        <div className="text-sm font-semibold text-foreground">ZK Object Fabric</div>
        <div className="text-[11px] text-muted-foreground">Tenant Console</div>
      </div>
    </div>
  );
}

function SideLink({ item, onNavigate }: { item: NavItem; onNavigate: () => void }) {
  const Icon = item.icon;
  return (
    <NavLink
      to={item.to}
      end={item.to === "/"}
      onClick={onNavigate}
      className={({ isActive }) =>
        cn(
          "flex items-center gap-2.5 rounded-md px-2.5 py-2 text-sm font-medium transition-colors",
          isActive
            ? "bg-primary/15 text-primary"
            : "text-muted-foreground hover:bg-muted hover:text-foreground",
        )
      }
    >
      <Icon className="size-4 shrink-0" />
      {item.label}
    </NavLink>
  );
}
