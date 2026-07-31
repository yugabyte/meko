// Left navigation rail. Renders the AMP brand mark, the WORKSPACE and
// AGENTS·MCP nav sections (driven entirely by navConfig), and a footer
// with the signed-in user + a sign-out menu. All styling comes from
// `layoutStyles` so the look stays consolidated in one place.

import { LogOut } from "lucide-react";
import { NavLink, useNavigate } from "react-router-dom";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { cn } from "@/lib/utils";
import { useAuth } from "@/auth/AuthContext";
import {
  AGENT_NAV,
  PLATFORM_MODEL,
  WORKSPACE_NAV,
  type NavItem,
} from "./navConfig";
import { useAgentPanel } from "./AgentPanel";
import { layoutStyles as s } from "./layoutStyles";

/** Shared inner content (icon + label + status dot) for a nav row. */
function NavRowInner({ item }: { item: NavItem }) {
  const Icon = item.icon;
  return (
    <>
      <Icon className={s.navIcon} />
      <span className={s.navLabel}>{item.label}</span>
      {item.online && <span className={s.statusDot} aria-label="online" />}
    </>
  );
}

function NavRow({ item }: { item: NavItem }) {
  const { open } = useAgentPanel();

  // Panel items (Voyager, Perf Advisor, Nexus) open the side panel
  // instead of navigating.
  if (item.panelId) {
    return (
      <button
        type="button"
        onClick={() => open(item.panelId!)}
        className={cn(s.navItem, s.navItemInactive, "w-full text-left")}
      >
        <NavRowInner item={item} />
      </button>
    );
  }

  return (
    <NavLink
      to={item.to!}
      className={({ isActive }) =>
        cn(s.navItem, isActive ? s.navItemActive : s.navItemInactive)
      }
    >
      <NavRowInner item={item} />
    </NavLink>
  );
}

/** First-letters initials for the avatar tile (max two). */
function initials(name: string): string {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]?.toUpperCase() ?? "")
    .join("");
}

export function Sidebar() {
  const navigate = useNavigate();
  const { user, signOut } = useAuth();

  const handleSignOut = () => {
    signOut();
    navigate("/signin", { replace: true });
  };

  return (
    <aside className={s.sidebar}>
      {/* Brand */}
      <div className={s.brand}>
        <div className={s.brandMark}>
          <span className={s.brandDot} />
        </div>
        <div className="min-w-0">
          <div className={s.brandName}>Yugabyte</div>
          <div className={s.brandTag}>Agentic Postgres Platform</div>
        </div>
      </div>

      {/* Nav */}
      <nav className={s.nav}>
        <div className={s.sectionLabel}>Workspace</div>
        <div className="flex flex-col gap-0.5">
          {WORKSPACE_NAV.map((item) => (
            <NavRow key={item.label} item={item} />
          ))}
        </div>

        <div className={s.sectionLabel}>Agents · MCP</div>
        <div className="flex flex-col gap-0.5">
          {AGENT_NAV.map((item) => (
            <NavRow key={item.label} item={item} />
          ))}
        </div>

        {/* Platform-model legend */}
        <div className={s.platformBox}>
          <div className={s.platformTitle}>Platform model</div>
          <div className={s.platformList}>
            {PLATFORM_MODEL.map((row) => (
              <div key={row.label} className={s.platformRow}>
                <span className={cn(s.platformSwatch, row.swatch)} />
                <span className={s.platformLabel}>{row.label}</span>
                <span className={s.platformMeta}>· {row.meta}</span>
              </div>
            ))}
          </div>
        </div>
      </nav>

      {/* Logged-in user */}
      <div className={s.footer}>
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button type="button" className={s.userRow}>
              <span className={s.userAvatar}>
                {user ? initials(user.name) : "?"}
              </span>
              <span className="min-w-0 flex-1">
                <span className={cn(s.userName, "block")}>
                  {user?.name ?? "Guest"}
                </span>
                <span className={cn(s.userMeta, "block")}>{user?.email}</span>
              </span>
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="w-56">
            <DropdownMenuLabel>{user?.email}</DropdownMenuLabel>
            <DropdownMenuSeparator />
            <DropdownMenuItem onSelect={handleSignOut}>
              <LogOut className="mr-2 h-4 w-4" />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </aside>
  );
}
