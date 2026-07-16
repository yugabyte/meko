// Admin console sidebar: brand logo block, primary nav (Data Plane / Users /
// Metrics / AMP), and a footer showing the signed-in user. App-level actions
// (theme toggle, sign out) live in AdminHeader, not here.
import { Server, Users, BarChart3, Database, LineChart } from "lucide-react";
import { NavLink } from "react-router-dom";
import type { LucideIcon } from "lucide-react";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { useAuth } from "@/lib/authContext";
import { isAmpAdminEnabled } from "@/lib/amp";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";
import { Separator } from "@/components/ui/separator";

type NavItem = { to: string; label: string; icon: LucideIcon; end?: boolean };

const navItemClass = ({ isActive }: { isActive: boolean }) =>
  cn(
    "flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors",
    isActive
      ? "bg-accent text-accent-foreground font-medium"
      : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
  );

// AuthUser carries only { sub, email }, so derive a friendly display name from
// the email's local-part ("first.last@x" -> "First Last") for the footer.
function displayNameFor(email: string): string {
  const local = email.split("@")[0] ?? email;
  const words = local.split(/[._-]+/).filter(Boolean);
  if (words.length === 0) return email;
  return words.map((w) => w[0].toUpperCase() + w.slice(1)).join(" ");
}

function initialsFor(name: string): string {
  const parts = name.split(/\s+/).filter(Boolean);
  if (parts.length === 0) return "?";
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[1][0]).toUpperCase();
}

export function AdminSidebar() {
  const { user } = useAuth();
  const ampEnabled = isAmpAdminEnabled();

  const navItems: NavItem[] = [
    { to: ROUTES.dataPlane, label: "Data Plane", icon: Server },
    { to: ROUTES.users, label: "Users & tiers", icon: Users },
    { to: ROUTES.metrics, label: "Metrics", icon: BarChart3 },
    { to: ROUTES.executive, label: "Executive Dashboard", icon: LineChart },
  ];
  if (ampEnabled) {
    navItems.push({ to: ROUTES.ampRegistration, label: "AMP", icon: Database });
  }

  return (
    <aside className="flex h-screen w-56 flex-col border-r border-sidebar-border bg-sidebar">
      <div className="flex h-14 items-center gap-2.5 px-4 border-b border-sidebar-border">
        <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-lg bg-brand text-brand-foreground font-bold text-base leading-none">
          M
        </div>
        <div className="flex flex-col leading-tight">
          <span className="font-semibold text-sm tracking-tight">MekoAdmin</span>
          <span className="text-[11px] text-muted-foreground">by YugabyteDB</span>
        </div>
      </div>

      <nav className="flex flex-1 flex-col gap-1 p-3">
        <p className="px-2 py-1 text-xs font-medium text-muted-foreground uppercase tracking-wider">
          Overview
        </p>
        {navItems.map(({ to, label, icon: Icon, end }) => (
          <NavLink key={to} to={to} end={end} className={navItemClass}>
            <Icon className="h-4 w-4" />
            {label}
          </NavLink>
        ))}
      </nav>

      {user && (
        <div className="p-3">
          <Separator className="mb-3" />
          <div className="flex items-center gap-2.5 px-1">
            <Avatar className="h-8 w-8">
              <AvatarFallback className="bg-brand/15 text-brand text-xs font-medium">
                {initialsFor(displayNameFor(user.email))}
              </AvatarFallback>
            </Avatar>
            <div className="flex min-w-0 flex-col leading-tight">
              <span className="truncate text-sm font-medium" title={user.email}>
                {displayNameFor(user.email)}
              </span>
              <span className="truncate text-xs text-muted-foreground" title={user.email}>
                {user.email}
              </span>
            </div>
          </div>
        </div>
      )}
    </aside>
  );
}
