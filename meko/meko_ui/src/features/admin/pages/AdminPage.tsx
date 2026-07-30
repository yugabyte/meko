// AdminPage — the in-app Admin console shell. Renders a horizontal sub-nav
// (Data Plane / Users & tiers / Metrics / Executive / AMP) and the active
// section below it, keyed off the `/admin/...` path. The whole surface is gated
// by SHOW_ADMIN_VIEW (see `@/lib/admin`) at the sidebar entry AND here, so a
// direct URL to /admin while the flag is off bounces back to Datapacks.
import { NavLink, Navigate, useLocation } from "react-router-dom";
import { Server, Users, BarChart3, LineChart, Database } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { ADMIN_SECTION, ROUTES, adminSectionPath } from "@/app/routes/paths";
import { isAdminViewEnabled } from "@/lib/admin";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { DataPlanePage } from "@/features/admin/pages/DataPlanePage";
import { UsersPage } from "@/features/admin/pages/UsersPage";
import { MetricsPage } from "@/features/admin/pages/MetricsPage";
import { ExecutiveDashboardPage } from "@/features/admin/pages/ExecutiveDashboardPage";
import { AmpRegistrationPage } from "@/features/admin/pages/AmpRegistrationPage";

type Section = { segment: string; label: string; icon: LucideIcon };

const SECTIONS: Section[] = [
  { segment: ADMIN_SECTION.dataPlane, label: "Foundation", icon: Server },
  { segment: ADMIN_SECTION.users, label: "Users & tiers", icon: Users },
  { segment: ADMIN_SECTION.metrics, label: "Metrics", icon: BarChart3 },
  { segment: ADMIN_SECTION.executive, label: "Executive Dashboard", icon: LineChart },
  { segment: ADMIN_SECTION.amp, label: "AMP", icon: Database },
];

// The active section is the last path segment after /admin; a bare /admin (or
// an unknown segment) resolves to Data Plane.
function activeSegment(pathname: string): string {
  const rest = pathname.slice(ROUTES.admin.length).replace(/^\/+/, "");
  const seg = rest.split("/").filter(Boolean)[0] ?? "";
  return SECTIONS.some((s) => s.segment === seg) ? seg : ADMIN_SECTION.dataPlane;
}

function renderSection(segment: string) {
  switch (segment) {
    case ADMIN_SECTION.users:
      return <UsersPage />;
    case ADMIN_SECTION.metrics:
      return <MetricsPage />;
    case ADMIN_SECTION.executive:
      return <ExecutiveDashboardPage />;
    case ADMIN_SECTION.amp:
      return <AmpRegistrationPage />;
    default:
      return <DataPlanePage />;
  }
}

export default function AdminPage() {
  const { pathname } = useLocation();

  // Defense-in-depth: the sidebar hides the Admin tab when the flag is off, but
  // a direct URL shouldn't render the console either.
  if (!isAdminViewEnabled()) return <Navigate to={ROUTES.datapacks} replace />;

  const active = activeSegment(pathname);

  return (
    <div className="flex min-h-0 w-full flex-1 flex-col bg-background text-foreground">
      {/* Same top bar as every other workspace page — theme toggle, invite
          bell, and "Signed in as …" with the tier badge. */}
      <AppWorkspaceHeader />
      <nav className="flex shrink-0 items-center gap-1 overflow-x-auto border-b border-border px-4 py-2">
        {SECTIONS.map(({ segment, label, icon: Icon }) => (
          <NavLink
            key={label}
            to={adminSectionPath(segment)}
            end={segment === ADMIN_SECTION.dataPlane}
            className={cn(
              "flex items-center gap-2 whitespace-nowrap rounded-md px-3 py-1.5 text-sm font-medium transition-colors",
              active === segment
                ? "bg-foreground/10 text-foreground"
                : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
            )}
          >
            <Icon className="h-4 w-4 shrink-0" />
            {label}
          </NavLink>
        ))}
      </nav>

      <div className="min-h-0 flex-1 overflow-y-auto">{renderSection(active)}</div>
    </div>
  );
}
