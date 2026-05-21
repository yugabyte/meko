import { NavLink, useLocation, useNavigate } from "react-router-dom";
import {
  Database,
  Activity,
  ChevronLeft,
  ChevronRight,
  LogOut,
  Download,
  ChevronDown,
  MessageSquare,
  PlayCircle,
  Sparkles,
  BookOpen,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { useEffect, useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  AUTH_LOGOUT_EVENT,
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  getCurrentUser,
  listDatapacks,
  logout,
  ONBOARDING_STATUS_CACHE_KEY,
  PROVISION_STATUS_CACHE_KEY,
  type CurrentUser,
  type Datapack,
} from "@/lib/mekoApi";
import { addAppEventListener, dispatchAppEvent, removeAppEventListener } from "@/lib/browserEvents";
import { ROUTES, datapackDetailPath } from "@/app/routes/paths";
import { FeedbackDialog } from "@/features/layout/FeedbackDialog";

export default function Sidebar() {
  const location = useLocation();
  const navigate = useNavigate();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<string | null>(null);
  const [datapackList, setDatapackList] = useState<Datapack[]>([]);
  const [datapacksExpanded, setDatapacksExpanded] = useState(true);
  const [feedbackOpen, setFeedbackOpen] = useState(false);
  // Loaded once for the Feedback dialog (account_id + email). Same `/auth/me`
  // endpoint other components hit; cheap and lets the dialog open instantly.
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);

  useEffect(() => {
    if (!feedbackOpen || currentUser !== null) return;
    const ac = new AbortController();
    void getCurrentUser({ signal: ac.signal })
      .then((u) => setCurrentUser(u))
      .catch(() => setCurrentUser(null));
    return () => ac.abort();
  }, [feedbackOpen, currentUser]);

  // Drop the cached user when the user signs out so the next sign-in re-fetches.
  useEffect(() => {
    const onLogout = () => setCurrentUser(null);
    addAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
    return () => removeAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
  }, []);

  // Derive the active datapack from the URL: the segment after /datapacks/.
  // Falls back to null on /home, /workbench-lm, /observe-hub, etc.
  const activeDatapack = useMemo(() => {
    const prefix = `${ROUTES.datapacks}/`;
    if (!location.pathname.startsWith(prefix)) return null;
    const rest = location.pathname.slice(prefix.length);
    const first = rest.split("/").filter(Boolean)[0];
    if (!first || first === "new") return null;
    const name = decodeURIComponent(first);
    const match = datapackList.find((d) => d.datapack_name === name);
    return { name, id: match?.datapack_id ?? null };
  }, [location.pathname, datapackList]);

  const onOnboardingRoute =
    location.pathname === ROUTES.onboarding || location.pathname.startsWith(`${ROUTES.onboarding}/`);
  const onHomeInstallRoute =
    location.pathname === ROUTES.home || location.pathname.startsWith(`${ROUTES.home}/`);
  const onDatapacksRoute = location.pathname.startsWith(ROUTES.datapacks);
  const onWorkbenchLMRoute =
    location.pathname === ROUTES.workbenchLm ||
    location.pathname.startsWith(`${ROUTES.workbenchLm}/`);
  const activeDatapackId = (new URLSearchParams(location.search).get("datapack_id") ?? "").trim();
  // Fetch once when entering the datapacks area, not on every `/datapacks/...` subpath change
  // (otherwise opening a datapack from overview triggers an extra listDatapacks call).
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
  }, [onDatapacksRoute]);

  useEffect(() => {
    const refresh = () => {
      void listDatapacks()
        .then((items) => setDatapackList(items))
        .catch(() => setDatapackList([]));
    };
    addAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refresh);
    return () => removeAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refresh);
  }, []);

  useEffect(() => {
    if (location.pathname.startsWith(`${ROUTES.datapacks}/`)) {
      setDatapacksExpanded(true);
    }
  }, [location.pathname]);

  useEffect(() => {
    if (onWorkbenchLMRoute) {
      setIsCollapsed(true);
    }
  }, [onWorkbenchLMRoute]);

  const expandSidebar = () => setIsCollapsed(false);

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

      <div className={cn("p-4 flex items-center", isCollapsed ? "justify-center px-0" : "")}>
        {isCollapsed ? (
          <img src="/favicons/favicon-32x32-dark.png" alt="meko" className="w-8 h-8 animate-in fade-in" />
        ) : (
          <img src="/meko-logo.svg" alt="meko" className="h-12 w-auto animate-in fade-in" />
        )}
      </div>

      <div className="flex-1 overflow-y-auto py-4 flex flex-col gap-1 px-3">
        {/* Welcome → /onboarding; Install (MCP setup) nested under same group as Home */}
        {isCollapsed ? (
          <>
            <NavLink
              to={ROUTES.onboarding}
              onClick={expandSidebar}
              className={cn(
                "flex items-center rounded-md text-sm font-medium transition-colors justify-center w-10 h-10 mx-auto",
                onOnboardingRoute
                  ? "bg-white/10 text-white"
                  : "text-gray-400 hover:bg-white/5 hover:text-white",
              )}
              title="Welcome"
            >
              <Sparkles className="h-5 w-5 shrink-0" />
            </NavLink>
            <NavLink
              to={ROUTES.home}
              onClick={expandSidebar}
              className={cn(
                "flex items-center rounded-md text-sm font-medium transition-colors justify-center w-10 h-10 mx-auto",
                onHomeInstallRoute
                  ? "bg-white/10 text-white"
                  : "text-gray-400 hover:bg-white/5 hover:text-white",
              )}
              title="Install"
            >
              <Download className="h-5 w-5 shrink-0" />
            </NavLink>
          </>
        ) : (
          <div className="flex flex-col gap-0.5">
            <NavLink
              to={ROUTES.onboarding}
              onClick={expandSidebar}
              className={cn(
                "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                onOnboardingRoute
                  ? "bg-white/10 text-white"
                  : "text-gray-400 hover:bg-white/5 hover:text-white",
              )}
            >
              <Sparkles className="h-4 w-4 shrink-0" />
              <span className="animate-in fade-in truncate">Welcome</span>
            </NavLink>
            <div className="mt-0.5 flex flex-col gap-0.5 border-l border-white/10 ml-4 pl-2">
              <NavLink
                to={ROUTES.home}
                onClick={expandSidebar}
                className={({ isActive }) =>
                  cn(
                    "flex items-center gap-2 rounded-md py-1.5 pl-2 pr-2 text-xs font-medium transition-colors min-w-0",
                    isActive
                      ? "bg-white/10 text-white"
                      : "text-gray-400 hover:bg-white/5 hover:text-white",
                  )
                }
              >
                <Download className="h-3.5 w-3.5 shrink-0 opacity-90" />
                <span className="truncate">Install</span>
              </NavLink>
            </div>
          </div>
        )}

        <NavLink
          to={ROUTES.workbenchLm}
          className={cn(
            "flex items-center rounded-md text-sm font-medium transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
            location.pathname === ROUTES.workbenchLm ||
              location.pathname.startsWith(`${ROUTES.workbenchLm}/`)
              ? "bg-white/10 text-white"
              : "text-gray-400 hover:bg-white/5 hover:text-white",
          )}
          title={isCollapsed ? "WorkbenchLM" : undefined}
        >
          <PlayCircle className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && <span className="animate-in fade-in truncate">WorkbenchLM</span>}
        </NavLink>

        {/* Datapacks + children */}
        {isCollapsed ? (
          <NavLink
            to={ROUTES.datapacks}
            onClick={expandSidebar}
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
                onClick={expandSidebar}
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
                    key={dp.datapack_id}
                    to={datapackDetailPath(dp.datapack_name, dp.datapack_id)}
                    title={dp.datapack_name}
                    className={() => {
                      const basePath = datapackDetailPath(dp.datapack_name);
                      const onDatapackSubroute =
                        location.pathname === basePath || location.pathname.startsWith(`${basePath}/`);
                      // Keep the datapack highlighted whenever the current route is
                      // under that datapack path. If a datapack_id query is present,
                      // also require id match to avoid ambiguity on stale links.
                      const idMatches = !activeDatapackId || activeDatapackId === dp.datapack_id;
                      const isActive = onDatapackSubroute && idMatches;
                      return cn(
                        "flex items-center rounded-md py-1.5 pl-2 pr-2 text-xs font-medium transition-colors min-w-0",
                        isActive
                          ? "bg-white/10 text-white"
                          : "text-gray-400 hover:bg-white/5 hover:text-white",
                      );
                    }}
                  >
                    <span className="truncate">{dp.datapack_name}</span>
                  </NavLink>
                ))}
              </div>
            ) : null}
          </div>
        )}

        {/* Observe */}
        <NavLink
          to={ROUTES.observeHub}
          onClick={expandSidebar}
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
        {/* Pre-GA marker. Uses the shared `Badge` (outline variant)
            from `@/components/ui/badge` to inherit the app-wide chip
            styling — same token outlines used by the wizard step
            badges and KB status pills. Hidden in collapsed mode
            because the narrow rail can't fit the label cleanly. */}
        {!isCollapsed ? (
          <Badge
            variant="outline"
            className="mb-2 self-start gap-1 border-amber-500/35 bg-amber-500/10 text-[10px] uppercase tracking-wider text-amber-200"
            title="Meko is in early access — features and APIs may change"
          >
            <Sparkles className="h-3 w-3" aria-hidden />
            Early access
          </Badge>
        ) : null}

        <button
          type="button"
          className={cn(
            "flex items-center rounded-md text-sm font-medium text-blue-300 ring-1 ring-blue-500/30 bg-blue-500/5 hover:bg-blue-500/15 hover:text-blue-100 transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          onClick={() => setFeedbackOpen(true)}
          title={isCollapsed ? "Send feedback" : undefined}
          aria-label="Send feedback"
        >
          <MessageSquare className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} />
          {!isCollapsed && <span className="animate-in fade-in truncate">Feedback</span>}
        </button>

        <a
          href="https://discord.com/invite/GWcSST7GqZ"
          target="_blank"
          rel="noopener noreferrer"
          className={cn(
            "group mt-2",
            "flex items-center rounded-md text-sm font-medium text-gray-400 hover:bg-white/5 hover:text-white transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          title={isCollapsed ? "Join us on Discord" : undefined}
          aria-label="Join us on Discord"
        >
          <img
            src="/discord-icon.svg"
            alt=""
            aria-hidden
            className={cn("shrink-0 opacity-40 group-hover:opacity-100 transition-opacity", isCollapsed ? "w-5 h-5" : "w-4 h-4")}
          />
          {!isCollapsed && <span className="animate-in fade-in truncate">Join us on Discord</span>}
        </a>

        <a
          href="https://docs.mekodata.ai/"
          target="_blank"
          rel="noopener noreferrer"
          className={cn(
            "flex items-center rounded-md text-sm font-medium text-gray-400 hover:bg-white/5 hover:text-white transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          title={isCollapsed ? "Docs" : undefined}
          aria-label="Open documentation"
        >
          <BookOpen className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} aria-hidden />
          {!isCollapsed && <span className="animate-in fade-in truncate">Docs</span>}
        </a>

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

      <FeedbackDialog
        open={feedbackOpen}
        onOpenChange={setFeedbackOpen}
        userId={currentUser?.account_id ?? null}
        email={currentUser?.email ?? null}
        currentPath={location.pathname}
        datapackId={activeDatapack?.id ?? null}
        datapackName={activeDatapack?.name ?? null}
      />
    </div>
  );
}
