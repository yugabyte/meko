import { Database, LayoutDashboard, Moon, Sun, LogOut } from "lucide-react";
import { NavLink } from "react-router-dom";
import { ROUTES } from "@/app/routes/paths";
import { cn } from "@/lib/utils";
import { useThemeToggle } from "@/lib/theme";
import { useAuth } from "@/lib/authContext";
import { isAmpAdminEnabled } from "@/lib/amp";
import { Button } from "@/components/ui/button";
import { Separator } from "@/components/ui/separator";

export function AdminSidebar() {
  const { theme, toggle } = useThemeToggle();
  const { user, logout } = useAuth();
  const ampEnabled = isAmpAdminEnabled();

  return (
    <aside className="flex h-screen w-56 flex-col border-r border-border bg-card">
      <div className="flex h-14 items-center gap-2 px-4 border-b border-border">
        <span className="font-semibold text-sm tracking-tight">Meko Admin</span>
      </div>

      <nav className="flex flex-1 flex-col gap-1 p-3">
        <p className="px-2 py-1 text-xs font-medium text-muted-foreground uppercase tracking-wider">
          Overview
        </p>
        <NavLink
          to={ROUTES.dashboard}
          end
          className={({ isActive }) =>
            cn(
              "flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors",
              isActive
                ? "bg-accent text-accent-foreground font-medium"
                : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
            )
          }
        >
          <LayoutDashboard className="h-4 w-4" />
          Dashboard
        </NavLink>
        {ampEnabled && (
          <NavLink
            to={ROUTES.ampRegistration}
            className={({ isActive }) =>
              cn(
                "flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors",
                isActive
                  ? "bg-accent text-accent-foreground font-medium"
                  : "text-muted-foreground hover:bg-accent hover:text-accent-foreground",
              )
            }
          >
            <Database className="h-4 w-4" />
            AMP
          </NavLink>
        )}
      </nav>

      <div className="p-3">
        <Separator className="mb-3" />
        {user && (
          <p className="px-2 pb-2 text-xs text-muted-foreground truncate" title={user.email}>
            {user.email}
          </p>
        )}
        <Button
          variant="ghost"
          size="sm"
          className="w-full justify-start gap-2 text-muted-foreground"
          onClick={toggle}
        >
          {theme === "dark" ? (
            <Sun className="h-4 w-4" />
          ) : (
            <Moon className="h-4 w-4" />
          )}
          {theme === "dark" ? "Light mode" : "Dark mode"}
        </Button>
        <Button
          variant="ghost"
          size="sm"
          className="w-full justify-start gap-2 text-muted-foreground"
          onClick={logout}
        >
          <LogOut className="h-4 w-4" />
          Sign out
        </Button>
      </div>
    </aside>
  );
}
