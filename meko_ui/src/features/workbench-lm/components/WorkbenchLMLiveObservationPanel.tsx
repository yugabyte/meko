// Right-rail "Live observation" panel for the WorkbenchLM.
// Three stacked sections (top 5 each):
//   - Learnings        (purple) — agent memories not yet promoted to KB.
//                                  Each tile has a "Promote to Ko" button.
//   - Collective Memory (sky)   — memories already promoted into the KB.
//   - Shared Knowledge  (emerald) — KB documents.
//
// Refresh sources:
//   1. `refreshTick` prop — bumped by the parent after each chat turn.
//   2. Internal periodic timer — bumps an internal tick every
//      `AUTO_REFRESH_INTERVAL_MS` so newly arriving memories /
//      conversations / KB updates surface without a manual click.
//      Pauses while the tab is hidden (Page Visibility API) so we
//      don't churn API calls in background tabs.
//   3. Hover/focus pause — while the user has their cursor inside the
//      panel or a tile is keyboard-focused, polling is suppressed so
//      a tile they're actively reading doesn't shift, vanish, or
//      flicker mid-read. Resumes the moment the cursor leaves.
//
// Tiles fade in via `animate-in fade-in slide-in-from-bottom-1` on mount.
// Combined with stable React keys, only *genuinely new* items animate;
// existing items stay put across re-renders.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Activity,
  ArrowUpToLine,
  Layers,
  Loader2,
  RefreshCw,
  Sparkles,
  Trash2,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  deleteMemory,
  formatMemoryAuthor,
  getApiErrorMessage,
  listMemories,
  listPromotedMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
  promoteMemories,
  type Memory,
  type PromotedMemory,
} from "@/lib/api/mekoApi";
import {
  isDatapackGrantEligibleToPromoteMemoryToKnowledge,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";

type Props = {
  datapackId: string | null;
  datapackName: string | null;
  /** Bumped by the parent (e.g. after a chat turn) to force refetch. */
  refreshTick?: number;
  datapackGrant?: DatapackShareGrant | null;
  paused?: boolean;
};

const TOP_N = 5;
/** How often the panel auto-refreshes its three sections while visible. */
const AUTO_REFRESH_INTERVAL_MS = 10_000;
/** How long the "extracting…" pulse stays lit after a chat turn. Matches the
 *  empirical Mem0 background-extractor window. */
const EXTRACTING_WINDOW_MS = 40_000;

export function WorkbenchLMLiveObservationPanel({
  datapackId,
  datapackName,
  refreshTick = 0,
  datapackGrant = null,
  paused = false,
}: Props) {
  // Internal ticker that bumps every AUTO_REFRESH_INTERVAL_MS while the tab
  // is visible AND the user isn't interacting with the panel. Combined with
  // `refreshTick` (parent-driven, post-chat-turn) so each child section
  // re-runs whenever EITHER source changes.
  const [autoTick, setAutoTick] = useState(0);
  // True while the cursor is inside the panel or a tile has keyboard focus
  // — used to defer polling so the tile a user is actively reading doesn't
  // shift / disappear out from under them.
  const [isInteracting, setIsInteracting] = useState(false);
  // Sticky-true once the user sends the first chat turn for this datapack.
  // Drives the "Extracting memories…" placeholder in the Memories section
  // while Mem0's background extractor is still working (typically 30–60s
  // after the turn lands). Resets whenever the datapack changes
  const [chatTurnOccurred, setChatTurnOccurred] = useState(false);
  // Time-bounded version of the same signal: lights up the small
  // "extracting…" pulse in the section headers for EXTRACTING_WINDOW_MS
  // after each turn. Resets on datapack change.
  const [extracting, setExtracting] = useState(false);
  useEffect(() => {
    setChatTurnOccurred(false);
    setExtracting(false);
  }, [datapackId]);
  useEffect(() => {
    if (refreshTick <= 0) return;
    setChatTurnOccurred(true);
    setExtracting(true);
    const t = setTimeout(() => setExtracting(false), EXTRACTING_WINDOW_MS);
    return () => clearTimeout(t);
  }, [refreshTick]);

  useEffect(() => {
    // Skip when:
    //  - the user is reading the panel (tile-shift prevention), OR
    //  - the WorkbenchLM route is not active (parent passes `paused`).
    // Effect re-runs when either flips, so polling resumes automatically.
    if (isInteracting || paused) return;
    let id: ReturnType<typeof setInterval> | null = null;
    const start = () => {
      if (id !== null) return;
      id = setInterval(() => setAutoTick((t) => t + 1), AUTO_REFRESH_INTERVAL_MS);
    };
    const stop = () => {
      if (id !== null) {
        clearInterval(id);
        id = null;
      }
    };
    const onVisibilityChange = () => {
      if (document.visibilityState === "visible") start();
      else stop();
    };
    if (document.visibilityState === "visible") start();
    document.addEventListener("visibilitychange", onVisibilityChange);
    return () => {
      document.removeEventListener("visibilitychange", onVisibilityChange);
      stop();
    };
  }, [isInteracting, paused]);

  // Effective tick passed to each section: either source bumps will
  // trigger their refetch effects.
  const effectiveTick = useMemo(
    () => refreshTick + autoTick,
    [refreshTick, autoTick],
  );

  return (
    <aside
      className="flex min-h-0 w-full flex-col border-l border-border bg-background"
      // Pause auto-poll while the user is reading. `mouseenter/leave` covers
      // pointer interaction; `focusin/out` covers keyboard nav (a11y).
      onMouseEnter={() => setIsInteracting(true)}
      onMouseLeave={() => setIsInteracting(false)}
      onFocus={() => setIsInteracting(true)}
      onBlur={(e) => {
        // Only release when focus actually leaves the panel — `onBlur` also
        // fires when focus moves between elements *inside* the aside.
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) {
          setIsInteracting(false);
        }
      }}
    >
      <div className="flex items-center justify-between border-b border-border/70 px-4 py-3">
        <div className="flex items-center gap-2">
          <Activity className="h-4 w-4 text-purple-400" aria-hidden />
          <h2 className="text-sm font-medium text-foreground">Live observation</h2>
        </div>
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              className={cn(
                "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide transition-colors",
                isInteracting
                  ? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-200"
                  : "border-purple-500/30 bg-purple-500/10 text-purple-700 dark:text-purple-200",
              )}
            >
              <span
                className={cn(
                  "h-1.5 w-1.5 rounded-full",
                  isInteracting ? "bg-amber-300" : "animate-pulse bg-purple-300",
                )}
                aria-hidden
              />
              {isInteracting ? "paused" : "live"}
            </span>
          </TooltipTrigger>
          <TooltipContent side="top" className="max-w-[260px] text-center">
            {isInteracting
              ? "Auto-refresh paused while you're reading a tile, so it doesn't shift or vanish mid-read. Resumes when your cursor leaves the panel."
              : "Auto-refreshes every 10 seconds while this tab is visible."}
          </TooltipContent>
        </Tooltip>
      </div>

      <div className="flex min-h-0 flex-1 flex-col divide-y divide-border/50 overflow-y-auto px-4">
        <div style={{ marginTop: '16px' }}>
          <LearningsSection
            datapackId={datapackId}
            datapackGrant={datapackGrant}
            refreshTick={effectiveTick}
            chatTurnOccurred={chatTurnOccurred}
            extracting={extracting}
            // Bump the internal poll tick so siblings (Collective Memory in
            // particular) refetch immediately after the user promotes a
            // memory — the new entry shows up there without waiting for
            // the next 10s tick.
            onPromoted={() => setAutoTick((t) => t + 1)}
          />
        </div>
        <div style={{ marginTop: '16px' }}>
          <CollectiveMemorySection
            datapackId={datapackId}
            refreshTick={effectiveTick}
            chatTurnOccurred={chatTurnOccurred}
            extracting={extracting}
          />
        </div>
        {/* <div style={{ marginTop: '16px' }}>
          <SharedKnowledgeSection
            datapackName={datapackName}
            refreshTick={effectiveTick}
          />
        </div> */}
      </div>
    </aside>
  );
}

type SectionShellProps = {
  title: string;
  icon: React.ReactNode;
  toneText: string;
  toneBadge: string;
  count?: number;
  /** Tone classes for the small "extracting…" chip rendered next to the
   *  count when a chat turn is being processed in the background. */
  extracting?: boolean;
  extractingChipTone?: string;
  loading: boolean;
  error: string | null;
  onRefresh: () => void;
  children: React.ReactNode;
};

function SectionShell({
  title,
  icon,
  toneText,
  toneBadge,
  count,
  extracting = false,
  extractingChipTone,
  loading,
  error,
  onRefresh,
  children,
}: SectionShellProps) {
  return (
    <section className="flex min-h-0 flex-col py-5 first:pt-4 last:pb-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="flex min-w-0 items-center gap-2">
          {icon}
          <h3 className={cn("text-[11px] font-semibold uppercase tracking-wider", toneText)}>
            {title}
          </h3>
          {count !== undefined ? (
            <span className={cn("rounded-full px-1.5 py-0.5 text-[10px] font-medium", toneBadge)}>
              {count}
            </span>
          ) : null}
          {extracting ? (
            <Tooltip>
              <TooltipTrigger asChild>
                <span
                  className={cn(
                    "inline-flex items-center gap-1 rounded-full border px-1.5 py-0.5 text-[9px] font-medium uppercase tracking-wider",
                    extractingChipTone ?? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-200",
                  )}
                  aria-live="polite"
                >
                  <Loader2 className="h-2.5 w-2.5 animate-spin" aria-hidden />
                  extracting
                </span>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[240px] text-center">
                Mem0 is still pulling memories out of your last message — usually takes a few seconds. New rows will appear here automatically.
              </TooltipContent>
            </Tooltip>
          ) : null}
        </div>
        <button
          type="button"
          onClick={onRefresh}
          disabled={loading}
          className="text-muted-foreground transition-colors hover:text-foreground disabled:opacity-50"
          aria-label={`Refresh ${title.toLowerCase()}`}
        >
          {loading ? (
            <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          ) : (
            <RefreshCw className="h-3.5 w-3.5" aria-hidden />
          )}
        </button>
      </div>
      {error ? (
        <p className="text-xs text-red-400">{error}</p>
      ) : (
        children
      )}
    </section>
  );
}

// --- Learnings (purple: matches CollectiveLearningPipeline learnings tile) ---
// Renamed from "Top memories" — these are agent memories that haven't been
// promoted to the shared knowledge base yet. Each tile shows a small
// "Promote to Ko" button; on success the tile is removed from this list.

// Theme-aware: light mode = soft purple-tinted card, dark mode = the
// brand-art dark gradient where `via-[#141228]` is a hand-tuned
// purple-tinted midpoint smoothing the transition from `purple-950/65`
// to `meko-navy-950`. The raw-hex midpoint is intentional — replacing
// with a neutral navy would lose the purple blend that makes the
// corner read as "purple section" against the other coloured tiles.
const MEMORY_TONE = {
  border: "border-purple-400 dark:border-purple-500/40",
  bg:
    "bg-gradient-to-br from-purple-50 via-card to-card " +
    "dark:from-purple-950/65 dark:via-[#141228] dark:to-meko-navy-950",
  text: "text-purple-700 dark:text-purple-200/95",
  badge:
    "border border-purple-500/30 bg-purple-500/10 text-purple-700 dark:text-purple-200",
};

function LearningsSection({
  datapackId,
  datapackGrant,
  refreshTick,
  chatTurnOccurred,
  extracting,
  onPromoted,
}: {
  datapackId: string | null;
  /** Caller's grant on the active datapack. Contributors see the
   *  Promote button disabled (with a tooltip explaining why); the
   *  server enforces the same rule so this is purely a UX hint. */
  datapackGrant: DatapackShareGrant | null;
  refreshTick: number;
  /** True once at least one chat turn has completed since the datapack was
   *  selected. Lets us swap the empty-state copy from "Add a conversation"
   *  to "Extracting memories…" while Mem0's extractor catches up. */
  chatTurnOccurred: boolean;
  /** True for the post-turn extraction window — drives the small
   *  "extracting" chip in the section header even when rows already exist. */
  extracting: boolean;
  /** Fired after a successful Promote to Ko so siblings (e.g. the
   *  Collective Memory section) can refetch immediately instead of
   *  waiting up to 10s for the next auto-poll. */
  onPromoted?: () => void;
}) {
  const [items, setItems] = useState<Memory[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // account_id — needed for promoteMemories and to gate the per-tile
  // delete button (only the author can delete). Resolved ONCE per session
  // via react-query (cached, `staleTime: Infinity`) instead of inside the
  // polled fetch below, so `/me` no longer rides along on every poll tick.
  const { data: currentUser } = useCurrentUser();
  const accountId = currentUser?.account_id ?? "";
  // Memory id currently being promoted (drives spinner + button disable).
  // Single-string state since one promote at a time keeps the UX predictable.
  const [promotingId, setPromotingId] = useState<string | null>(null);
  // Memory id currently being deleted (same single-flight pattern).
  const [deletingId, setDeletingId] = useState<string | null>(null);

  const canPromote = isDatapackGrantEligibleToPromoteMemoryToKnowledge({
    grant: datapackGrant ?? undefined,
  });

  const fetchMemories = useCallback(
    async (signal?: AbortSignal) => {
      // Wait for the cached identity before listing — listMemories is
      // scoped by user_id. While it's resolving (first paint) we simply
      // skip; the effect re-runs once `accountId` lands.
      if (!datapackId || !accountId) {
        setItems([]);
        setTotal(0);
        return;
      }
      setLoading(true);
      setError(null);
      try {
        const resp = await listMemories(
          {
            datapack_id: datapackId,
            user_id: accountId,
            user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
            page: 1,
            page_size: TOP_N,
          },
          { signal },
        );
        setItems(resp.memories ?? []);
        setTotal(resp.total ?? 0);
      } catch (err) {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setError(getApiErrorMessage(err, "Failed to load learnings."));
        setItems([]);
        setTotal(0);
      } finally {
        setLoading(false);
      }
    },
    [datapackId, accountId],
  );

  useEffect(() => {
    const ac = new AbortController();
    void fetchMemories(ac.signal);
    return () => ac.abort();
  }, [fetchMemories, refreshTick]);

  const handlePromote = useCallback(
    async (memoryId: string) => {
      if (!datapackId || !accountId || promotingId || !canPromote) return;
      setPromotingId(memoryId);
      try {
        await promoteMemories({
          user_id: accountId,
          datapack_id: datapackId,
          memory_ids: [memoryId],
        });
        // Remove the just-promoted tile so the user sees instant feedback.
        // The next auto-poll will reconcile against the server.
        setItems((prev) => prev.filter((m) => m.id !== memoryId));
        setTotal((prev) => Math.max(0, prev - 1));
        toast({ title: "Promoted to knowledge" });
        // Tell the parent so the Collective Memory section refetches now
        // (otherwise the just-promoted entry doesn't surface there until
        // the next 10s auto-poll tick).
        onPromoted?.();
      } catch (err) {
        toast({
          title: "Couldn't promote",
          description: getApiErrorMessage(err, "Promotion failed."),
        });
      } finally {
        setPromotingId(null);
      }
    },
    [accountId, promotingId, canPromote, onPromoted],
  );

  const handleDelete = useCallback(
    async (memoryId: string) => {
      if (!datapackId || deletingId) return;
      setDeletingId(memoryId);
      try {
        await deleteMemory(memoryId, datapackId);
        // Server confirmed the delete — drop the tile locally so the UI
        // updates without waiting up to 10s for the next auto-poll. On
        // failure we never reach this line, so no rollback is needed.
        setItems((prev) => prev.filter((m) => m.id !== memoryId));
        setTotal((prev) => Math.max(0, prev - 1));
        toast({ title: "Memory deleted" });
      } catch (err) {
        toast({
          title: "Couldn't delete",
          description: getApiErrorMessage(err, "Delete failed."),
        });
      } finally {
        setDeletingId(null);
      }
    },
    [datapackId, deletingId],
  );

  return (
    <SectionShell
      title="Memories"
      icon={<Sparkles className="h-3.5 w-3.5 text-violet-400" aria-hidden />}
      toneText={MEMORY_TONE.text}
      toneBadge={MEMORY_TONE.badge}
      count={datapackId ? Math.min(items.length, total) : undefined}
      extracting={Boolean(datapackId) && extracting}
      extractingChipTone="border-purple-500/40 bg-purple-500/10 text-purple-700 dark:text-purple-200"
      loading={loading}
      error={error}
      onRefresh={() => void fetchMemories()}
    >
      {!datapackId ? (
        <p className="text-xs text-muted-foreground">Select a datapack to see learnings.</p>
      ) : loading && items.length === 0 ? (
        <p className="text-xs text-muted-foreground">Loading memories…</p>
      ) : items.length === 0 ? (
        chatTurnOccurred ? (
          // Mem0's background extractor takes 30–60s after each turn. The
          // panel auto-polls every 10s so the placeholder swaps to real
          // tiles as soon as the extractor finishes.
          <p className="inline-flex items-center gap-2 text-xs text-purple-700 dark:text-purple-200/80">
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
            Extracting learnings from your recent chat...
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">Add a conversation to view memories.</p>
        )
      ) : (
        <ul className="space-y-2">
          {items.slice(0, TOP_N).map((m) => {
            const isPromoting = promotingId === m.id;
            const isDeleting = deletingId === m.id;
            // Promote is disabled while any tile is in flight, before the
            // account_id is resolved, or when the caller's grant doesn't
            // allow promotion (contributors). Server enforces the same gate.
            const promoteDisabled = promotingId !== null || !accountId || !canPromote;
            const promoteTooltip = canPromote
              ? "Promote this memory to the shared knowledge base"
              : "Contributors can't promote memories. Ask the datapack owner or a maintainer.";
            // Only the author of a memory may delete it. In a shared
            // datapack other members appear in the list; those tiles
            // render without the delete affordance.
            const isMine = Boolean(m.user_id) && m.user_id === accountId;
            // Centralised fallback chain (name → email → short UUID → "-")
            // lives in formatMemoryAuthor; we pass `selfLabel: "you"` so
            // your own rows read "by you" in this personal-session view.
            const authorLabel = formatMemoryAuthor(m, {
              currentUserId: accountId,
              selfLabel: "you",
              fallback: "",
            });
            return (
              <li
                key={m.id}
                className={cn(
                  "rounded-lg border px-3 py-2 animate-in fade-in slide-in-from-bottom-1 duration-300",
                  MEMORY_TONE.border,
                  MEMORY_TONE.bg,
                )}
                title={m.data}
                tabIndex={0}
              >
                <p className="line-clamp-2 text-xs text-foreground dark:text-white/95">{m.data}</p>
                {authorLabel ? (
                  <p className="mt-0.5 truncate text-[10px] text-purple-700 dark:text-purple-200/60">
                    by {authorLabel}
                  </p>
                ) : null}
                <div className="mt-1.5 flex items-center justify-between gap-2">
                  <span className="truncate text-[10px] text-purple-700 dark:text-purple-200/70">
                    {m.agent_id ? `agent · ${m.agent_id}` : " "}
                  </span>
                  <div className="flex shrink-0 items-center gap-1">
                    {/* {isMine ? (
                      <button
                        type="button"
                        onClick={() => void handleDelete(m.id)}
                        disabled={deletingId !== null}
                        className={cn(
                          "inline-flex items-center gap-1 rounded-md border px-2 py-0.5 text-[10px] font-medium transition-colors",
                          "border-red-500/40 bg-red-500/10 text-red-700 dark:text-red-200 hover:bg-red-500/20",
                          "disabled:cursor-not-allowed disabled:opacity-50",
                        )}
                        title="Delete this memory (only you can delete your own)"
                        aria-label="Delete memory"
                      >
                        {isDeleting ? (
                          <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                        ) : (
                          <Trash2 className="h-3 w-3" aria-hidden />
                        )}
                      </button>
                    ) : null} */}
                    <button
                      type="button"
                      onClick={() => void handlePromote(m.id)}
                      disabled={promoteDisabled}
                      className={cn(
                        // "Subtle in light, saturated in dark" pattern:
                        // light mode is a soft purple-tint pill with
                        // dark purple text (reads as an accent chip
                        // rather than a heavy CTA against the white
                        // tile); dark mode is the original saturated
                        // purple-500 + white text. Matches the rest of
                        // the promote-action family (`<MemoryPromotionPanel>`,
                        // `<FileUpload>`).
                        "inline-flex items-center gap-1 rounded-md px-2 py-0.5 text-[10px] font-medium transition-colors",
                        "bg-purple-100 hover:bg-purple-200 text-purple-700",
                        "dark:bg-purple-500 dark:hover:bg-purple-600 dark:text-white",
                        "disabled:cursor-not-allowed disabled:opacity-50",
                      )}
                      title={promoteTooltip}
                    >
                      {isPromoting ? (
                        <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                      ) : (
                        <ArrowUpToLine className="h-3 w-3" aria-hidden />
                      )}
                      {isPromoting ? "Promoting…" : "Promote to Ko"}
                    </button>
                  </div>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </SectionShell>
  );
}

// --- Collective Memory (sky: distinct from blue/memories + purple/learnings
// + emerald/knowledge so each section has its own accent). Pulls memories
// that have already been promoted into the tenant knowledge base. ---

// Theme-aware sky-tinted tile. Mirrors `MEMORY_TONE`'s structure (soft
// card in light, brand-art gradient in dark). `via-[#0e1c2a]` is the
// hand-tuned sky-tinted dark midpoint — kept as raw hex for the same
// brand-art reason as the purple gradient above.
const COLLECTIVE_TONE = {
  border: "border-sky-400 dark:border-sky-500/40",
  bg:
    "bg-gradient-to-br from-sky-50 via-card to-card " +
    "dark:from-sky-950/65 dark:via-[#0e1c2a] dark:to-meko-navy-950",
  text: "text-sky-700 dark:text-sky-200/95",
  badge:
    "border border-sky-500/30 bg-sky-500/10 text-sky-700 dark:text-sky-200",
};

function CollectiveMemorySection({
  datapackId,
  refreshTick,
  chatTurnOccurred,
  extracting,
}: {
  datapackId: string | null;
  refreshTick: number;
  /** Lets us swap the empty-state hint once a chat turn has fired —
   *  promotion needs memories upstream, so a "still extracting" line
   *  reads better than the static "Promote a learning" copy. */
  chatTurnOccurred: boolean;
  /** Mirrors Learnings — shows a small extracting chip in the header for
   *  the post-turn extraction window. */
  extracting: boolean;
}) {
  const [items, setItems] = useState<PromotedMemory[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchPromoted = useCallback(
    async (signal?: AbortSignal) => {
      if (!datapackId) {
        setItems([]);
        setTotal(0);
        return;
      }
      setLoading(true);
      setError(null);
      try {
        const resp = await listPromotedMemories(
          { datapack_id: datapackId, page: 1, page_size: TOP_N },
          { signal },
        );
        setItems(resp.promoted_memories ?? []);
        setTotal(resp.total ?? 0);
      } catch (err) {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setError(getApiErrorMessage(err, "Failed to load collective memory."));
        setItems([]);
        setTotal(0);
      } finally {
        setLoading(false);
      }
    },
    [datapackId],
  );

  useEffect(() => {
    const ac = new AbortController();
    void fetchPromoted(ac.signal);
    return () => ac.abort();
  }, [fetchPromoted, refreshTick]);

  return (
    <SectionShell
      title="Collective Memories"
      icon={<Layers className="h-3.5 w-3.5 text-sky-700 dark:text-sky-300" aria-hidden />}
      toneText={COLLECTIVE_TONE.text}
      toneBadge={COLLECTIVE_TONE.badge}
      count={datapackId ? Math.min(items.length, total) : undefined}
      extracting={Boolean(datapackId) && extracting}
      extractingChipTone="border-sky-500/40 bg-sky-500/10 text-sky-700 dark:text-sky-200"
      loading={loading}
      error={error}
      onRefresh={() => void fetchPromoted()}
    >
      {!datapackId ? (
        <p className="text-xs text-muted-foreground">Select a datapack to see collective memory.</p>
      ) : loading && items.length === 0 ? (
        <p className="text-xs text-muted-foreground">Loading collective memory…</p>
      ) : items.length === 0 ? (
        chatTurnOccurred ? (
          // Collective memory needs upstream memories to exist first — so
          // right after a turn the more honest message is "wait for Mem0,
          // then promote." We surface the extraction-in-progress hint too.
          <p className="inline-flex items-center gap-2 text-xs text-sky-700 dark:text-sky-200/80">
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
            Extracting memories from your recent chat. Promote one above to seed collective memory.
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">
            Nothing here yet. Promote a memory above to seed collective memory.
          </p>
        )
      ) : (
        <ul className="space-y-2">
          {items.slice(0, TOP_N).map((m) => (
            <li
              key={m.id}
              className={cn(
                "rounded-lg border px-3 py-2 animate-in fade-in slide-in-from-bottom-1 duration-300",
                COLLECTIVE_TONE.border,
                COLLECTIVE_TONE.bg,
              )}
              title={m.data}
              tabIndex={0}
            >
              <p className="line-clamp-2 text-xs text-foreground dark:text-white/95">{m.data}</p>
              {m.agent_id ? (
                <p className="mt-1 truncate text-[10px] text-sky-700 dark:text-sky-200/70">
                  agent · {m.agent_id}
                </p>
              ) : null}
            </li>
          ))}
        </ul>
      )}
    </SectionShell>
  );
}
