import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ArrowUpRight,
  Book,
  ChevronDown,
  ChevronUp,
  Database,
  Link as LinkIcon,
  Plus,
  Sparkles,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  getDatapackProvisionStatus,
  listDatapacks, // imperative call in the provisioning-poll loop only
  type CurrentUser,
  type Datapack,
  type ProvisionStatus,
} from "@/lib/api/mekoApi";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { FREE_TIER_BLOCKED_MESSAGES, getFreeTierStatus } from "@/lib/freeTier";
import { FreeTierLimitBanner } from "@/lib/freeTierBanner";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import {
  DEFAULT_ACCOUNT_DATAPACK_NAME,
  datapackDetailPath,
  datapackDetailUrlOpenConnect,
  datapackNewConfigurePath,
  datapackSharePath,
} from "@/app/routes/paths";
import { queryClient } from "@/lib/queryClient";
import { DATAPACKS_QUERY_KEY, useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import DatapacksTilesSection from "@/features/datapacks/components/DatapacksTilesSection";
import { AccountTokenBurnTile } from "@/features/datapacks/components/efficiency/TokenBurnTile";
import { CONTEXTUAL_PROMINENCE_SEARCH_ENABLED } from "@/lib/tokenSavings";
import { DatapackConnectDialog } from "@/features/datapacks/components/datapack-detail/dialogs";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";

// Local key for tracking whether the user has seen the "What is a Datapack?"
// explainer at least once. Used to expand the panel by default on the very
// first visit, then collapse on subsequent visits (the user can still toggle).
// Page-local — not centralised because it's only ever read/written here.
const DATAPACKS_OVERVIEW_INTRO_SEEN_KEY = "meko:datapacks-overview-intro-seen:v1";

type OverviewProps = {
  currentUser: CurrentUser | null;
};

export default function DatapacksOverviewPage({ currentUser }: OverviewProps) {
  const navigate = useNavigate();
  // Shared react-query cache (see `useDatapacks`). Sidebar + Connect +
  // WorkbenchLM + ObservePage all subscribe to the same key — first
  // visit to any of them fetches once, every other surface gets the
  // result for free until `staleTime` (30s) elapses or
  // `DATAPACKS_LIST_UPDATED_EVENT` invalidates it.
  const { data: datapacks = [], isLoading, error: datapacksQueryError } = useDatapacks();
  // Free-tier usage counters — drive the "Add Datapack" cap below.
  // Hook errors fail soft (status defaults to all-false on undefined),
  // so a usage-API outage doesn't lock users out of creating datapacks
  // — the BE re-enforces the cap on POST /datapacks if it's hit.
  const { data: usage } = useAccountUsage();
  const freeTier = useMemo(
    () => getFreeTierStatus(usage, currentUser?.user_type),
    [usage, currentUser?.user_type],
  );
  const [isProvisioningDefaultDatapack, setIsProvisioningDefaultDatapack] = useState(false);
  const [pollError, setPollError] = useState<string | null>(null);
  const error =
    pollError ??
    (datapacksQueryError
      ? getApiErrorMessage(datapacksQueryError, "Failed to load datapacks.")
      : null);
  // First-visit treatment: expand the explainer by default if the user
  // has never seen it.
  //
  // CRITICAL: the state initialiser MUST be pure — read-only.
  //
  // React 18 Strict Mode (and concurrent rendering more generally)
  // intentionally invokes state initialisers more than once to surface
  // impure behaviour. Previously this initialiser also called
  // `localStorage.setItem(..., "true")` to mark the explainer as seen.
  // That looked fine — but on the second Strict-Mode invocation the
  // initialiser would re-read the now-`"true"` flag and return `false`,
  // so the first visit collapsed instead of expanded.

  const [isExplanationExpanded, setIsExplanationExpanded] = useState<boolean>(() => {
    try {
      return localStorage.getItem(DATAPACKS_OVERVIEW_INTRO_SEEN_KEY) !== "true";
    } catch {
      return false;
    }
  });

  // Side-effect partner to the initialiser above: persist the
  // "seen-once" flag after the first render commits, so subsequent
  // visits start collapsed. Guarded so we don't bother writing if the
  // flag is already set.
  useEffect(() => {
    try {
      if (localStorage.getItem(DATAPACKS_OVERVIEW_INTRO_SEEN_KEY) !== "true") {
        localStorage.setItem(DATAPACKS_OVERVIEW_INTRO_SEEN_KEY, "true");
      }
    } catch {
      // private-mode / disabled-storage: silently noop.
    }
  }, []);

  // Bridge `DATAPACKS_LIST_UPDATED_EVENT` (fired by every page that
  // mutates the list — create / delete / rename / share-accept) into
  // a react-query cache invalidation. All consumers of `useDatapacks()`
  // refetch once collectively, instead of each subscriber maintaining
  // its own listener.
  useEffect(() => {
    const refresh = () => {
      setPollError(null);
      void queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] });
    };
    addAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refresh);
    return () => removeAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, refresh);
  }, []);

  // Brand-new accounts: the api_server starts provisioning their default
  // datapack on first hit, so the very first GET /datapacks may return
  // empty. Poll until the row appears (or give up after 60s) and write
  // through the react-query cache so every other surface picks up the
  // fresh list automatically.
  useEffect(() => {
    if (isLoading || datapacks.length > 0) {
      setIsProvisioningDefaultDatapack(false);
      return;
    }

    let mounted = true;
    let attempts = 0;
    const maxAttempts = 30;
    setIsProvisioningDefaultDatapack(true);

    const poll = async () => {
      attempts += 1;
      if (!mounted) return;
      try {
        const items = await listDatapacks();
        if (!mounted) return;
        if (items.length > 0) {
          // Write into the shared cache instead of a local setState.
          // Every consumer of `useDatapacks()` re-renders with the new
          // list — Sidebar, Connect, WorkbenchLM, this page.
          queryClient.setQueryData<Datapack[]>([DATAPACKS_QUERY_KEY], items);
          setIsProvisioningDefaultDatapack(false);
          return;
        }
      } catch {
        // keep polling
      }
      if (attempts >= maxAttempts && mounted) {
        setIsProvisioningDefaultDatapack(false);
      }
    };

    const intervalId = globalThis.setInterval(poll, 2000);
    void poll();

    return () => {
      mounted = false;
      globalThis.clearInterval(intervalId);
      setIsProvisioningDefaultDatapack(false);
    };
  }, [isLoading, datapacks.length]);

  /** Shared rows: not owner (`grant` ≠ owner) and invite grant viewer | contributor | maintainer. */
  const sharedWithYouCount = useMemo(
    () =>
      datapacks.filter((dp) => {
        if (isDatapackListGrantOwner(dp)) return false;
        return true;
      }).length,
    [datapacks],
  );

  // "Fresh user" = nothing they created themselves yet. Either zero datapacks
  // or only the auto-provisioned `meko_default_datapack`. Used to surface a
  // gentle docs nudge so brand-new users have an explicit "I have no idea
  // what this is — point me at the docs" affordance.
  const isFreshUser = useMemo(() => {
    if (isLoading) return false;
    if (datapacks.length === 0) return true;
    return datapacks.length === 1 && datapacks[0]?.datapack_name === DEFAULT_ACCOUNT_DATAPACK_NAME;
  }, [isLoading, datapacks]);

  // Workspace-level Connect dialog state. "Connect" is a Meko-wide action,
  // not a per-datapack one — we just need *some* provisioned datapack the
  // user owns to mint MCP/server credentials behind the scenes. Pick the
  // first owner row from the list; that's the user's de-facto "home"
  // datapack (default_datapack for fresh users, the oldest one they made
  // otherwise).
  const firstOwnerDatapack = useMemo(
    () => datapacks.find((dp) => isDatapackListGrantOwner(dp)) ?? null,
    [datapacks],
  );
  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [connectProvisionStatus, setConnectProvisionStatus] = useState<ProvisionStatus | null>(
    null,
  );

  // Fetch provision status lazily on open so we don't burn a request on
  // every overview-page visit. Mirrors the per-detail-page pattern.
  //
  // We reset `connectProvisionStatus` to `null` on every (re)open and on
  // the "no owner datapack" early-return so stale data from a previous
  // datapack/session can't briefly leak into the dialog before the new
  // fetch resolves. The dialog's render priority is loading → error →
  // status, so a stale `status` wouldn't show today — but a future
  // tweak to the priority order would silently regress without this
  // defensive reset.
  useEffect(() => {
    if (!isConnectOpen) return;
    const dpId = firstOwnerDatapack?.datapack_id;
    if (!dpId) {
      setConnectProvisionStatus(null);
      setConnectError("No owner datapack available to connect.");
      setConnectLoading(false);
      return;
    }
    const ac = new AbortController();
    setConnectProvisionStatus(null);
    setConnectLoading(true);
    setConnectError(null);
    void getDatapackProvisionStatus(dpId, { signal: ac.signal })
      .then((status) => {
        if (ac.signal.aborted) return;
        setConnectProvisionStatus(status);
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        if ((err as { name?: string })?.name === "CanceledError") return;
        setConnectError(getApiErrorMessage(err, "Failed to load connection details."));
      })
      .finally(() => {
        if (!ac.signal.aborted) setConnectLoading(false);
      });
    return () => ac.abort();
  }, [isConnectOpen, firstOwnerDatapack?.datapack_id]);

  const showProvisioningScreen =
    !isLoading && datapacks.length === 0 && isProvisioningDefaultDatapack;

  if (showProvisioningScreen) {
    return (
      <div className="flex-1 min-h-screen bg-background text-foreground flex items-center justify-center px-6">
        <div className="w-full max-w-xl rounded-xl border border-blue-500/40 bg-blue-500/10 px-6 py-8 text-center">
          <div className="mx-auto mb-4 h-8 w-8 animate-spin rounded-full border-2 border-blue-300 border-t-transparent" />
          <h2 className="text-lg font-semibold text-blue-700 dark:text-blue-100">
            Setting up your default datapack...
          </h2>
          <p className="mt-2 text-sm text-blue-700 dark:text-blue-200/80">
            This may take a minute. We will show your datapack as soon as it is ready.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        <div className="space-y-4">
          <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
            <div className="flex flex-col gap-2">
              <div className="flex items-center gap-3">
                <div
                  className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-blue-500/15 ring-1 ring-blue-500/20"
                  aria-hidden
                >
                  <Database className="h-5 w-5 text-blue-400" />
                </div>
                <h1 className="text-2xl font-semibold tracking-tight text-foreground">Datapacks</h1>
              </div>
              <p className="text-muted-foreground text-sm max-w-xl leading-relaxed">
                All datapacks in your workspace. Open one to manage agents, knowledge, memories, and
                connection details.
              </p>
            </div>
            <div className="flex flex-wrap items-center gap-2 shrink-0">
              {/* Workspace-level Connect: opens DatapackConnectDialog using
                  the user's first owner datapack under the hood, but hides
                  the datapack name in the dialog because "Connect to Meko"
                  is the conceptual frame here, not "Connect to <name>". */}
              <Button
                type="button"
                size="lg"
                variant="outline"
                className="gap-2 border-border bg-background/80 font-medium"
                onClick={() => setIsConnectOpen(true)}
                disabled={isLoading || !firstOwnerDatapack}
                title={
                  !isLoading && !firstOwnerDatapack
                    ? "No owner datapack available to connect."
                    : undefined
                }
              >
                <LinkIcon className="w-4 h-4 shrink-0" aria-hidden />
                Connect
              </Button>
              {/* Free-tier datapack cap. Tooltip mounts only when the
                  cap is hit, so Radix has a well-formed (trigger +
                  content) tree in both states. Disabled button is
                  wrapped in a span so hover events still fire. */}
              {freeTier.hasHitDatapackCount ? (
                <Tooltip>
                  <TooltipTrigger asChild>
                    <span className="inline-flex">
                      <Button
                        size="lg"
                        className="gap-2 font-medium bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-70"
                        onClick={() => navigate(datapackNewConfigurePath())}
                        disabled
                      >
                        <Plus className="w-5 h-5 shrink-0" aria-hidden />
                        Add Datapack
                      </Button>
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top" className="max-w-[280px] text-center">
                    {FREE_TIER_BLOCKED_MESSAGES.datapack}
                  </TooltipContent>
                </Tooltip>
              ) : (
                <Button
                  size="lg"
                  className="gap-2 font-medium bg-primary text-primary-foreground hover:bg-primary/90 disabled:opacity-70"
                  onClick={() => navigate(datapackNewConfigurePath())}
                >
                  <Plus className="w-5 h-5 shrink-0" aria-hidden />
                  Add Datapack
                </Button>
              )}
            </div>
          </div>

          {/* <div className="flex flex-wrap items-center justify-between gap-4 rounded-xl border border-border bg-card/50 px-4 py-3 text-sm">
            <div className="flex min-w-0 items-center gap-2 border-border sm:border-r sm:pr-4">
              <Layers className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
              <span className="font-medium text-foreground">Workspace total</span>
            </div>
            <div className="text-muted-foreground">
              <span className="tabular-nums text-foreground/90">{sharedWithYouCount}</span>
              {" shared with me"}
            </div>
          </div> */}
        </div>

        {error ? (
          <div className="rounded-md border border-red-500/40 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-200">
            {error}
          </div>
        ) : null}
        {isProvisioningDefaultDatapack ? (
          <div className="rounded-md border border-blue-500/40 bg-blue-500/10 px-4 py-3 text-sm text-blue-700 dark:text-blue-200">
            Setting up your default datapack...
          </div>
        ) : null}

        <div className="relative overflow-hidden rounded-xl border border-border bg-card">
          <div className="absolute top-0 right-0 w-64 h-64 bg-blue-500/10 rounded-full blur-3xl -translate-y-1/2 translate-x-1/3" />
          <div className="absolute bottom-0 left-0 w-64 h-64 bg-purple-500/10 rounded-full blur-3xl translate-y-1/3 -translate-x-1/3" />

          <div className="relative z-10">
            <div
              className="flex items-center justify-between p-4 sm:px-10 cursor-pointer hover:bg-white/5 transition-colors"
              onClick={() => setIsExplanationExpanded(!isExplanationExpanded)}
            >
              <div className="flex items-center gap-3">
                <Sparkles className="w-5 h-5 text-blue-400" />
                <h2 className="text-xl font-semibold text-foreground">What is a Datapack?</h2>
              </div>
              <Button
                variant="ghost"
                size="icon"
                className="text-muted-foreground hover:text-white"
              >
                {isExplanationExpanded ? (
                  <ChevronUp className="w-5 h-5" />
                ) : (
                  <ChevronDown className="w-5 h-5" />
                )}
              </Button>
            </div>

            {isExplanationExpanded ? (
              <div className="p-4 sm:px-10 sm:pb-10 pt-0 flex flex-col md:flex-row items-center gap-8 animate-in fade-in slide-in-from-top-4 duration-300">
                <div className="flex-1">
                  <p className="text-muted-foreground text-base mb-3 leading-relaxed max-w-2xl">
                    A datapack is everything your agent knows in one place, usually organized by a
                    project topic. Conversations you have with your agents, the memories they form,
                    the docs you upload, and a traceable record of what each agent did. Create one
                    per topic or project. Any AI tool you connect over MCP can read and write to it,
                    so your context follows you when you switch tools.
                  </p>
                  <p className="mb-6 max-w-2xl text-xs italic text-muted-foreground/70">
                    Under the hood, each datapack runs on a YugabyteDB database.
                  </p>
                  <div className="flex items-center gap-3">
                    <Button
                      variant="secondary"
                      className="gap-2 text-sm bg-foreground/10 hover:bg-foreground/20 text-foreground border-0"
                      onClick={() => window.open("https://docs.mekodata.ai/", "_blank")}
                    >
                      <Book className="w-4 h-4" />
                      Documentation
                    </Button>
                    <Button
                      variant="ghost"
                      className="gap-2 text-sm text-blue-400 hover:text-blue-700 dark:text-blue-300 hover:bg-blue-500/10"
                      onClick={() => window.open("https://docs.mekodata.ai/examples/", "_blank")}
                    >
                      View Examples
                    </Button>
                  </div>
                </div>

                {/* Decorative chip — reinforces the storage story already
                    spelled out in the caption below the body copy.
                    YugabyteDB is the actual storage layer; "Postgres-wire-
                    compatible" is the relevant compatibility note, not the
                    primary identity. The earlier "PostgreSQL → Logical DB
                    Mapping → Via YugabyteDB" framing inverted that and
                    read as if we ran on Postgres. */}
                <div className="hidden md:flex w-48 h-38 border border-border bg-input rounded-xl flex-col items-center justify-center gap-4 relative overflow-hidden group">
                  <div className="absolute inset-0 bg-gradient-to-tr from-blue-500/5 to-transparent opacity-0 group-hover:opacity-100 transition-opacity" />
                  <Database className="w-10 h-10 text-blue-400" />
                  <div className="text-center">
                    <div className="text-sm font-medium text-foreground">YugabyteDB</div>
                    <div className="text-xs text-muted-foreground mt-1">Multi-tenant storage</div>
                    <div className="mt-2 flex justify-center">
                      <span className="rounded-md border border-border/70 bg-card/90 px-2 py-0.5 font-mono text-[10px] leading-tight text-muted-foreground/90">
                        PostgreSQL-compatible
                      </span>
                    </div>
                  </div>
                </div>
              </div>
            ) : null}
          </div>
        </div>

        {/* First-run docs nudge — visible only while the user has nothing
            but the auto-provisioned default datapack. The Sidebar already
            has a "Read the docs" link, but a fresh user on this page often
            isn't sure where to start; surfacing the same link inline next
            to their (essentially empty) tile grid gives them an obvious
            "I'm new, just take me to the docs" path. */}
        {isFreshUser ? (
          <a
            href="https://docs.mekodata.ai/"
            target="_blank"
            rel="noopener noreferrer"
            className="group inline-flex items-center gap-1.5 text-sm text-blue-600 hover:text-blue-700 dark:text-blue-300 dark:hover:text-blue-200"
          >
            New here? Read the docs
            <ArrowUpRight
              className="h-3.5 w-3.5 shrink-0 opacity-70 transition-transform group-hover:-translate-y-0.5 group-hover:translate-x-0.5 group-hover:opacity-100"
              aria-hidden
            />
          </a>
        ) : null}

        {freeTier.hasHitDatapackCount ? (
          <FreeTierLimitBanner kind="datapack" className="mb-4" />
        ) : null}

        {/* Meko efficiency — account-wide savings across every datapack. The
            grid has room for the future Context Quality / Infra tiles. Only
            shown where contextual prominence search (which produces the
            token-savings data) is enabled. */}
        {CONTEXTUAL_PROMINENCE_SEARCH_ENABLED ? (
          <section className="space-y-3">
            <h2 className="text-sm font-medium uppercase tracking-wide text-muted-foreground">
              Meko efficiency
            </h2>
            <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
              <AccountTokenBurnTile />
            </div>
          </section>
        ) : null}

        <DatapacksTilesSection
          datapacks={datapacks}
          isLoading={isLoading}
          currentUser={currentUser}
          onOpenDatapack={(dp) => navigate(datapackDetailPath(dp.datapack_id))}
          onConnectDatapack={(dp) =>
            navigate(datapackDetailUrlOpenConnect(dp.datapack_id))
          }
          onShareDatapack={(dp) => navigate(datapackSharePath(dp.datapack_id))}
          onNewDatapack={() => navigate(datapackNewConfigurePath())}
        />

        {/* Workspace-level Connect dialog. `showDatapackName={false}` hides
            the "Datapack: <name>" row because the user clicked a Meko-wide
            Connect button — they didn't pick a specific datapack, so surfacing
            our internal pick (firstOwnerDatapack) would be misleading. The
            credentials shown (MCP URL + tokens) are still resolved through
            that datapack on the wire. */}
        <DatapackConnectDialog
          open={isConnectOpen}
          onOpenChange={(open) => {
            setIsConnectOpen(open);
            if (!open) {
              setConnectError(null);
              setConnectLoading(false);
            }
          }}
          loading={connectLoading}
          error={connectError}
          provisionStatus={connectProvisionStatus}
          showDatapackName={false}
        />
      </main>
    </div>
  );
}
