import { Link, NavLink, useLocation, useNavigate } from "react-router-dom";
import {
  Database,
  Activity,
  ArrowUpRight,
  ChevronRight,
  LogOut,
  Download,
  ChevronDown,
  MessageSquare,
  PanelLeftClose,
  PanelLeftOpen,
  PlayCircle,
  Sparkles,
  BookOpen,
  Users,
} from "lucide-react";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";
import { cn } from "@/lib/utils";
import { useEffect, useMemo, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  AUTH_LOGOUT_EVENT,
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  listDatapacks,
  logout,
  ONBOARDING_STATUS_CACHE_KEY,
  PROVISION_STATUS_CACHE_KEY,
  type Datapack,
} from "@/lib/mekoApi";
import { queryClient } from "@/lib/queryClient";
import { CURRENT_USER_QUERY_KEY, useCurrentUser } from "@/hooks/useCurrentUser";
import { addAppEventListener, dispatchAppEvent, removeAppEventListener } from "@/lib/browserEvents";
import { removeLocalItem, removeSessionItem } from "@/lib/browserStorage";
import { ROUTES, datapackDetailPath } from "@/app/routes/paths";
import { FeedbackDialog } from "@/features/layout/FeedbackDialog";

/**
 * Cap the nested "Datapacks" list at this many rows before it starts to
 * scroll internally. Without a cap, a user with 30+ datapacks pushes
 * everything below — Observe, Feedback, Sign out — off-screen unless
 * they scroll the whole sidebar.
 */
const MAX_VISIBLE_DATAPACKS = 10;

/**
 * Approximate height of one rendered row in the datapacks list.
 * `py-1.5` adds 12px of vertical padding; `text-xs` text + the
 * `gap-0.5` between siblings round the per-row footprint to ~28px.
 * Driving the cap off this constant means changing the row chrome
 * automatically rescopes the scroll threshold.
 */
const DATAPACK_ROW_HEIGHT_PX = 28;

export default function Sidebar() {
  const location = useLocation();
  const navigate = useNavigate();
  const [isCollapsed, setIsCollapsed] = useState(false);
  const [loggingOut, setLoggingOut] = useState(false);
  const [logoutError, setLogoutError] = useState<string | null>(null);
  const [datapackList, setDatapackList] = useState<Datapack[]>([]);
  const [datapacksExpanded, setDatapacksExpanded] = useState(true);
  const [feedbackOpen, setFeedbackOpen] = useState(false);
  // For the Feedback dialog (account_id + email). Shared session-cached
  // identity — only fetched when the dialog opens, and reuses the same
  // `/auth/me` cache entry other components hit (no extra request).
  const { data: currentUser = null } = useCurrentUser({ enabled: feedbackOpen });

  // Clear the shared `/me` cache when the user signs out so the next
  // sign-in (possibly as a different user) re-fetches instead of serving
  // the stale cached identity. One removal here clears it for every
  // component reading `useCurrentUser`, since they share one query key.
  useEffect(() => {
    const onLogout = () => queryClient.removeQueries({ queryKey: [CURRENT_USER_QUERY_KEY] });
    addAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
    return () => removeAppEventListener(AUTH_LOGOUT_EVENT, onLogout);
  }, []);

  // Derive the active datapack from the URL: the segment after /datapacks/.
  // Falls back to null on /connect, /workbench-lm, /observe-hub, etc.
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
  const onConnectRoute =
    location.pathname === ROUTES.connect || location.pathname.startsWith(`${ROUTES.connect}/`);
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
      removeSessionItem(PROVISION_STATUS_CACHE_KEY);
      removeLocalItem(ONBOARDING_STATUS_CACHE_KEY);
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
        // `bg-background` keeps the sidebar continuous with the
        // `<AppWorkspaceHeader>` (also `bg-background/95`) so the top
        // bar and left rail read as one connected workspace chrome,
        // separated from the main content only by the right border.
        // In dark mode this resolves to navy-900 — matches the
        // original design where the sidebar was the deepest surface.
        "flex-shrink-0 border-r border-border bg-background h-screen flex flex-col transition-all duration-300 relative",
        isCollapsed ? "w-[68px]" : "w-[240px]",
      )}
    >
      <Button
        variant="ghost"
        size="icon"
        // The collapse toggle sits ON the sidebar's right edge —
        // `bg-card` so it reads as a raised pill against both the
        // sidebar (background) on its left and the page (also
        // background) on its right.
        //
        // Icon choice: we deliberately use `PanelLeftClose` /
        // `PanelLeftOpen` (lucide) instead of a bare ChevronLeft /
        // ChevronRight. A standalone left chevron on a floating pill
        // reads almost universally as a browser-style "back" button —
        // users were clicking it expecting to navigate to the previous
        // page. The panel icons literally show a left-rail sidebar with
        // an inward/outward indicator, which matches the actual
        // affordance and is the convention used by VS Code, Cursor,
        // ChatGPT, etc.
        className="absolute -right-4 top-6 z-50 h-8 w-8 rounded-full border border-border bg-card shadow-md hover:bg-muted text-muted-foreground hover:text-foreground"
        onClick={() => setIsCollapsed(!isCollapsed)}
        title={isCollapsed ? "Expand sidebar" : "Collapse sidebar"}
        aria-label={isCollapsed ? "Expand sidebar" : "Collapse sidebar"}
        aria-expanded={!isCollapsed}
      >
        {isCollapsed ? (
          <PanelLeftOpen className="h-4 w-4" aria-hidden />
        ) : (
          <PanelLeftClose className="h-4 w-4" aria-hidden />
        )}
      </Button>

      <div className={cn("p-4 flex items-center", isCollapsed ? "justify-center px-0" : "")}>
        {/* Clickable logo → /datapacks (the workspace "home"). Standard
            convention across web apps: top-left brand mark returns the
            user to their main surface. Wrapped as a single `Link` so
            both collapsed (favicon) and expanded (full logo) states get
            the same affordance with one source of truth. */}
        <Link
          to={ROUTES.datapacks}
          className="inline-flex items-center rounded-md outline-none focus-visible:ring-2 focus-visible:ring-foreground/40"
          aria-label="Go to Datapacks"
        >
          {isCollapsed ? (
            <img src="/favicons/favicon-32x32-dark.png" alt="meko" className="w-8 h-8 animate-in fade-in" />
          ) : (
            <>
              {/*
                Render both logo variants and toggle visibility on the
                `.dark` class so the right one shows without a render
                flicker on theme change. `meko-logo.svg` is white-on-
                transparent (needs dark bg); `Meko_Logo_Blk.svg` is
                black-on-transparent (needs light bg). Tailwind's `dark:`
                variant is wired to `.dark` ancestor — see `index.css`'s
                `@custom-variant dark` declaration.
              */}
              <img
                src="/Meko_Logo_Blk.svg"
                alt="meko"
                className="h-12 w-auto animate-in fade-in block dark:hidden"
              />
              <img
                src="/meko-logo.svg"
                alt="meko"
                className="h-12 w-auto animate-in fade-in hidden dark:block"
              />
            </>
          )}
        </Link>
      </div>

      <div className="flex-1 overflow-y-auto py-4 flex flex-col gap-1 px-3">
        {/* Top-level nav: Welcome and Connect are siblings (Connect was
            previously nested under Welcome — promoted so it's reachable
            in one click from any page, since it's the user's primary
            "how do I plug Meko into my agent?" surface). */}
        {isCollapsed ? (
          <>
            <NavLink
              to={ROUTES.onboarding}
              onClick={expandSidebar}
              className={cn(
                "flex items-center rounded-md text-sm font-medium transition-colors justify-center w-10 h-10 mx-auto",
                onOnboardingRoute
                  ? "bg-foreground/10 text-foreground"
                  : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
              )}
              title="Welcome"
            >
              <Sparkles className="h-5 w-5 shrink-0" />
            </NavLink>
            <NavLink
              to={ROUTES.connect}
              onClick={expandSidebar}
              className={cn(
                "flex items-center rounded-md text-sm font-medium transition-colors justify-center w-10 h-10 mx-auto",
                onConnectRoute
                  ? "bg-foreground/10 text-foreground"
                  : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
              )}
              title="Connect"
            >
              <Download className="h-5 w-5 shrink-0" />
            </NavLink>
          </>
        ) : (
          <>
            <NavLink
              to={ROUTES.onboarding}
              onClick={expandSidebar}
              className={cn(
                "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                onOnboardingRoute
                  ? "bg-foreground/10 text-foreground"
                  : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
              )}
            >
              <Sparkles className="h-4 w-4 shrink-0" />
              <span className="animate-in fade-in truncate">Welcome</span>
            </NavLink>
            <NavLink
              to={ROUTES.connect}
              onClick={expandSidebar}
              className={cn(
                "flex items-center gap-3 rounded-md px-3 py-2 text-sm font-medium transition-colors",
                onConnectRoute
                  ? "bg-foreground/10 text-foreground"
                  : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
              )}
            >
              <Download className="h-4 w-4 shrink-0" />
              <span className="animate-in fade-in truncate">Connect</span>
            </NavLink>
          </>
        )}

        <NavLink
          to={ROUTES.workbenchLm}
          className={cn(
            "flex items-center rounded-md text-sm font-medium transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
            location.pathname === ROUTES.workbenchLm ||
              location.pathname.startsWith(`${ROUTES.workbenchLm}/`)
              ? "bg-foreground/10 text-foreground"
              : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
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
                ? "bg-foreground/10 text-foreground"
                : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
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
                    ? "bg-foreground/10 text-foreground"
                    : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
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
                  "h-auto w-9 shrink-0 rounded-l-none rounded-r-md px-0 text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
                  isDatapacksSectionActive ? "text-foreground" : "",
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
              <div
                className="flex flex-col gap-0.5 border-l border-white/10 ml-4 pl-2 mt-0.5 overflow-y-auto"
                style={{ maxHeight: MAX_VISIBLE_DATAPACKS * DATAPACK_ROW_HEIGHT_PX }}
              >
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
                          ? "bg-foreground/10 text-foreground"
                          : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
                      );
                    }}
                  >
                    {/*
                      `flex-1` on the name span makes it occupy all the
                      free horizontal space inside the row. That pushes
                      the share icon flush to the right edge regardless
                      of name length — without it, short names left the
                      icon floating mid-row right next to the text.
                    */}
                    <span className="min-w-0 flex-1 truncate">{dp.datapack_name}</span>
                    {!isDatapackListGrantOwner(dp) ? (
                      <Users
                        className="ml-1.5 h-3 w-3 shrink-0 text-purple-700 dark:text-purple-300/80"
                        aria-label="Shared with you"
                      />
                    ) : null}
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
              ? "bg-foreground/10 text-foreground"
              : "text-muted-foreground hover:bg-foreground/5 hover:text-foreground",
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
            className="mb-2 self-start gap-1 border-amber-500/35 bg-amber-500/10 text-[10px] uppercase tracking-wider text-amber-700 dark:text-amber-200"
            title="Meko is in early access — features and APIs may change"
          >
            <Sparkles className="h-3 w-3" aria-hidden />
            Early access
          </Badge>
        ) : null}

        {/* Docs lives at the top of this group so first-time users have
            an obvious "I don't know what this product is — where do I read
            up?" exit. The subtle ring + the trailing ↗ both signal "this
            opens in a new tab and is your friend." */}
        <a
          href="https://docs.mekodata.ai/"
          target="_blank"
          rel="noopener noreferrer"
          className={cn(
            "group flex items-center rounded-md text-sm font-medium text-foreground ring-1 ring-border bg-foreground/[0.03] hover:bg-foreground/10 hover:ring-foreground/30 transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          title={isCollapsed ? "Read the docs" : undefined}
          aria-label="Read the docs (opens in a new tab)"
        >
          <BookOpen className={cn("shrink-0", isCollapsed ? "w-5 h-5" : "w-4 h-4")} aria-hidden />
          {!isCollapsed && (
            <>
              <span className="animate-in fade-in truncate">Read the docs</span>
              <ArrowUpRight
                className="ml-auto h-3.5 w-3.5 shrink-0 opacity-60 group-hover:opacity-100 transition-opacity"
                aria-hidden
              />
            </>
          )}
        </a>

        <button
          type="button"
          className={cn(
            "mt-2 flex items-center rounded-md text-sm font-medium text-blue-700 dark:text-blue-300 ring-1 ring-blue-500/30 bg-blue-500/5 hover:bg-blue-500/15 hover:text-blue-700 dark:text-blue-100 transition-colors",
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
            "flex items-center rounded-md text-sm font-medium text-muted-foreground hover:bg-foreground/5 hover:text-foreground transition-colors",
            isCollapsed ? "justify-center w-10 h-10 mx-auto" : "gap-3 px-3 py-2",
          )}
          title={isCollapsed ? "Join us on Discord" : undefined}
          aria-label="Join us on Discord"
        >
          <img
            src="/discord-icon.svg"
            alt=""
            aria-hidden
            // The SVG ships as solid white per the brand-icons
            // convention (see CLAUDE.md). On a light page background
            // that's invisible — `invert` flips white → black in light
            // mode; `dark:invert-0` cancels the inversion in dark mode
            // so the original white is preserved.
            className={cn(
              "shrink-0 opacity-40 group-hover:opacity-100 transition-opacity invert dark:invert-0",
              isCollapsed ? "w-5 h-5" : "w-4 h-4",
            )}
          />
          {!isCollapsed && <span className="animate-in fade-in truncate">Join us on Discord</span>}
        </a>

        <button
          type="button"
          className={cn(
            "mt-1 flex items-center rounded-md text-sm font-medium text-muted-foreground hover:bg-foreground/5 hover:text-foreground transition-colors",
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
