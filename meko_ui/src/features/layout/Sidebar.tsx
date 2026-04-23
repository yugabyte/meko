import { NavLink, useLocation, useNavigate } from "react-router-dom";
import {
  Database,
  Activity,
  Box,
  ChevronLeft,
  ChevronRight,
  LogOut,
  Home,
  ChevronDown,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import {
  AUTH_LOGOUT_EVENT,
  getApiErrorMessage,
  listDatapacks,
  logout,
  ONBOARDING_STATUS_CACHE_KEY,
  PROVISION_STATUS_CACHE_KEY,
  type Datapack,
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { ROUTES, datapackDetailPath } from "@/app/routes/paths";

export default function Sidebar() {
  const location = useLocation();
  const navigate = useNavigate();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<string | null>(null);
  const [datapackList, setDatapackList] = useState<Datapack[]>([]);
  const [datapacksExpanded, setDatapacksExpanded] = useState(true);

  const onDatapacksRoute = location.pathname.startsWith(ROUTES.datapacks);

  useEffect(() => {
    if (!onDatapacksRoute) return;
    let cancelled = false;
    void listDatapacks()
      .then((items) => {
        if (!cancelled) setDatapackList(items);
      })
      .catch(() => {
        if (!cancelled) setDatapackList([]);
      });
    return () => {
      cancelled = true;
    };
  }, [onDatapacksRoute, location.pathname]);

  useEffect(() => {
    if (location.pathname.startsWith(`${ROUTES.datapacks}/`)) {
      setDatapacksExpanded(true);
    }
  }, [location.pathname]);

  const handleLogout = async () => {
    setLogoutError(null);
    setLoggingOut(true);
    try {
      await logout();
      sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
      localStorage.removeItem(ONBOARDING_STATUS_CACHE_KEY);
      dispatchAppEvent(AUTH_LOGOUT_EVENT);
      navigate(ROUTES.login, { replace: true });
    } catch (err) {
      setLogoutError(getApiErrorMessage(err, "Could not sign out. Please try again."));
      setLoggingOut(false);
    }
  };

  const isDatapacksSectionActive =
    location.pathname === ROUTES.datapacks || location.pathname.startsWith(`${ROUTES.datapacks}/`);

  return (
    <div
      className={cn(
        "flex-shrink-0 border-r border-border bg-[#0E1525] h-screen flex flex-col transition-all duration-300 relative",
        isCollapsed ? "w-[68px]" : "w-[240px]",
      )}
    >
      <Button
        variant="ghost"
        size="icon"
        className="absolute -right-4 top-6 z-50 h-8 w-8 rounded-full border border-border bg-[#0E1525] shadow-md hover:bg-muted text-muted-foreground hover:text-white"
        onClick={() => setIsCollapsed(!isCollapsed)}
      >
        {isCollapsed ? <ChevronRight className="h-4 w-4" /> : <ChevronLeft className="h-4 w-4" />}
      </Button>

      <div className={cn("p-4 flex items-center", isCollapsed ? "justify-center px-0" : "gap-3")}>
        <div className="w-8 h-8 rounded-md flex shrink-0 items-center justify-center bg-white text-black">
          <Box className="w-5 h-5" />
        </div>
        {!isCollapsed && (
          <span className="font-semibold text-xl tracking-tight text-white animate-in fade-in">meko</span>
        )}
      </div>

      <div className="flex-1 overflow-y-auto py-4 flex flex-col gap-1 px-3">
        {/* Home */}
        <NavLink
          to={ROUTES.home}
          className={cn(
            "flex items-center rounded-md text-sm font-medium transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
            location.pathname === ROUTES.home || location.pathname.startsWith(`${ROUTES.home}/`)
              ? "bg-white/10 text-white"
              : "text-gray-400 hover:bg-white/5 hover:text-white",
          )}
          title={isCollapsed ? "Home" : undefined}
        >
          <Home className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && <span className="animate-in fade-in truncate">Home</span>}
        </NavLink>

        {/* Datapacks + children */}
        {isCollapsed ? (
          <NavLink
            to={ROUTES.datapacks}
            className={cn(
              "flex items-center rounded-md text-sm font-medium transition-colors justify-center w-10 h-10 mx-auto",
              isDatapacksSectionActive
                ? "bg-white/10 text-white"
                : "text-gray-400 hover:bg-white/5 hover:text-white",
            )}
            title="Datapacks"
          >
            <Database className="w-5 h-5 shrink-0" />
          </NavLink>
        ) : (
          <div className="flex flex-col gap-0.5">
            <div className="flex items-stretch gap-0 rounded-md">
              <NavLink
                to={ROUTES.datapacks}
                className={cn(
                  "flex flex-1 items-center gap-3 rounded-l-md px-3 py-2 text-sm font-medium transition-colors min-w-0",
                  isDatapacksSectionActive
                    ? "bg-white/10 text-white"
                    : "text-gray-400 hover:bg-white/5 hover:text-white",
                )}
              >
                <Database className="w-4 h-4 shrink-0" />
                <span className="animate-in fade-in truncate">Datapacks</span>
              </NavLink>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                className={cn(
                  "h-auto w-9 shrink-0 rounded-l-none rounded-r-md px-0 text-gray-400 hover:bg-white/5 hover:text-white",
                  isDatapacksSectionActive ? "text-white" : "",
                )}
                aria-expanded={datapacksExpanded}
                aria-label={datapacksExpanded ? "Collapse datapack list" : "Expand datapack list"}
                onClick={(e) => {
                  e.preventDefault();
                  setDatapacksExpanded((v) => !v);
                }}
              >
                {datapacksExpanded ? (
                  <ChevronDown className="h-4 w-4 shrink-0" />
                ) : (
                  <ChevronRight className="h-4 w-4 shrink-0" />
                )}
              </Button>
            </div>
            {datapacksExpanded ? (
              <div className="flex flex-col gap-0.5 border-l border-white/10 ml-4 pl-2 mt-0.5">
                {datapackList.map((dp) => (
                  <NavLink
                    key={dp.id}
                    to={datapackDetailPath(dp.name)}
                    title={dp.name}
                    className={({ isActive }) =>
                      cn(
                        "flex items-center rounded-md py-1.5 pl-2 pr-2 text-xs font-medium transition-colors min-w-0",
                        isActive
                          ? "bg-white/10 text-white"
                          : "text-gray-400 hover:bg-white/5 hover:text-white",
                      )
                    }
                  >
                    <span className="truncate">{dp.name}</span>
                  </NavLink>
                ))}
              </div>
            ) : null}
          </div>
        )}

        {/* Observe */}
        <NavLink
          to={ROUTES.observeHub}
          className={cn(
            "flex items-center rounded-md text-sm font-medium transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
            location.pathname === ROUTES.observeHub || location.pathname.startsWith(`${ROUTES.observeHub}/`)
              ? "bg-white/10 text-white"
              : "text-gray-400 hover:bg-white/5 hover:text-white",
          )}
          title={isCollapsed ? "Observe" : undefined}
        >
          <Activity className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && <span className="animate-in fade-in truncate">Observe</span>}
        </NavLink>
      </div>

      <div className="p-3 flex flex-col gap-1">
        <button
          type="button"
          className={cn(
            "mt-1 flex items-center rounded-md text-sm font-medium text-gray-400 hover:bg-white/5 hover:text-white transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          onClick={handleLogout}
          disabled={loggingOut}
          title={isCollapsed ? "Sign out" : undefined}
        >
          <LogOut className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && (
            <span className="animate-in fade-in truncate">
              {loggingOut ? "Signing out..." : "Sign out"}
            </span>
          )}
        </button>
        {!isCollapsed && logoutError ? (
          <p className="px-3 pt-1 text-xs text-red-400">{logoutError}</p>
        ) : null}
      </div>
    </div>
  );
}
