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
  BookOpen,
  FileText,
  Layers,
  Loader2,
  RefreshCw,
  Sparkles,
} from "lucide-react";
import { cn } from "@/lib/utils";
import { toast } from "@/hooks/use-toast";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import {
  getApiErrorMessage,
  getCurrentUser,
  getKnowledgeBaseDocuments,
  listMemories,
  listPromotedMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
  promoteMemories,
  type KnowledgeBaseDocument,
  type Memory,
  type PromotedMemory,
} from "@/lib/mekoApi";

type Props = {
  datapackId: string | null;
  datapackName: string | null;
  /** Bumped by the parent (e.g. after a chat turn) to force refetch. */
  refreshTick?: number;
};

const TOP_N = 5;
/** How often the panel auto-refreshes its three sections while visible. */
const AUTO_REFRESH_INTERVAL_MS = 10_000;

export function WorkbenchLMLiveObservationPanel({
  datapackId,
  datapackName,
  refreshTick = 0,
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
  // after the turn lands). Resets whenever the datapack changes — a fresh
  // datapack should fall back to the standard "Add a conversation" copy.
  const [chatTurnOccurred, setChatTurnOccurred] = useState(false);
  useEffect(() => {
    setChatTurnOccurred(false);
  }, [datapackId]);
  useEffect(() => {
    if (refreshTick > 0) setChatTurnOccurred(true);
  }, [refreshTick]);

  useEffect(() => {
    // Skip while the user is reading the panel — resume on next render
    // when isInteracting flips back to false (this effect re-runs).
    if (isInteracting) return;
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
  }, [isInteracting]);

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
          <h2 className="text-sm font-medium text-white">Live observation</h2>
        </div>
        <Tooltip>
          <TooltipTrigger asChild>
            <span
              className={cn(
                "inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide transition-colors",
                isInteracting
                  ? "border-amber-500/30 bg-amber-500/10 text-amber-200"
                  : "border-purple-500/30 bg-purple-500/10 text-purple-200",
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
            refreshTick={effectiveTick}
            chatTurnOccurred={chatTurnOccurred}
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
  loading,
  error,
  onRefresh,
  children,
}: SectionShellProps) {
  return (
    <section className="flex min-h-0 flex-col py-5 first:pt-4 last:pb-4">
      <div className="mb-3 flex items-center justify-between">
        <div className="flex items-center gap-2">
          {icon}
          <h3 className={cn("text-[11px] font-semibold uppercase tracking-wider", toneText)}>
            {title}
          </h3>
          {count !== undefined ? (
            <span className={cn("rounded-full px-1.5 py-0.5 text-[10px] font-medium", toneBadge)}>
              {count}
            </span>
          ) : null}
        </div>
        <button
          type="button"
          onClick={onRefresh}
          disabled={loading}
          className="text-muted-foreground transition-colors hover:text-white disabled:opacity-50"
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

const MEMORY_TONE = {
  border: "border-purple-500/40",
  bg: "bg-gradient-to-br from-purple-950/65 via-[#141228] to-[#0b1220]",
  text: "text-purple-200/95",
  badge: "border border-purple-500/30 bg-purple-500/10 text-purple-200",
};

function LearningsSection({
  datapackId,
  refreshTick,
  chatTurnOccurred,
  onPromoted,
}: {
  datapackId: string | null;
  refreshTick: number;
  /** True once at least one chat turn has completed since the datapack was
   *  selected. Lets us swap the empty-state copy from "Add a conversation"
   *  to "Extracting memories…" while Mem0's extractor catches up. */
  chatTurnOccurred: boolean;
  /** Fired after a successful Promote to Ko so siblings (e.g. the
   *  Collective Memory section) can refetch immediately instead of
   *  waiting up to 10s for the next auto-poll. */
  onPromoted?: () => void;
}) {
  const [items, setItems] = useState<Memory[]>([]);
  const [total, setTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // account_id captured during fetch — needed for promoteMemories.
  const [accountId, setAccountId] = useState<string>("");
  // Memory id currently being promoted (drives spinner + button disable).
  // Single-string state since one promote at a time keeps the UX predictable.
  const [promotingId, setPromotingId] = useState<string | null>(null);

  const fetchMemories = useCallback(
    async (signal?: AbortSignal) => {
      if (!datapackId) {
        setItems([]);
        setTotal(0);
        return;
      }
      setLoading(true);
      setError(null);
      try {
        const user = await getCurrentUser({ signal });
        setAccountId(user.account_id);
        const resp = await listMemories(
          {
            datapack_id: datapackId,
            user_id: user.account_id,
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
    [datapackId],
  );

  useEffect(() => {
    const ac = new AbortController();
    void fetchMemories(ac.signal);
    return () => ac.abort();
  }, [fetchMemories, refreshTick]);

  const handlePromote = useCallback(
    async (memoryId: string) => {
      if (!datapackId || !accountId || promotingId) return;
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
    [accountId, datapackId, promotingId, onPromoted],
  );

  return (
    <SectionShell
      title="Memories"
      icon={<Sparkles className="h-3.5 w-3.5 text-violet-400" aria-hidden />}
      toneText={MEMORY_TONE.text}
      toneBadge={MEMORY_TONE.badge}
      count={datapackId ? Math.min(items.length, total) : undefined}
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
          <p className="inline-flex items-center gap-2 text-xs text-purple-200/80">
            <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
            Extracting learnings from your recent chat — usually 30–60 seconds.
          </p>
        ) : (
          <p className="text-xs text-muted-foreground">Add a conversation to view memories.</p>
        )
      ) : (
        <ul className="space-y-2">
          {items.slice(0, TOP_N).map((m) => {
            const isPromoting = promotingId === m.id;
            const promoteDisabled = promotingId !== null || !accountId;
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
                <p className="line-clamp-2 text-xs text-white/95">{m.data}</p>
                <div className="mt-1.5 flex items-center justify-between gap-2">
                  <span className="truncate text-[10px] text-purple-200/70">
                    {m.agent_id ? `agent · ${m.agent_id}` : " "}
                  </span>
                  <button
                    type="button"
                    onClick={() => void handlePromote(m.id)}
                    disabled={promoteDisabled}
                    className={cn(
                      // Match MemoryPromotionPanel's "Promote to Knowledge"
                      // styling (purple-600 / purple-700) so the action
                      // looks the same wherever it shows up.
                      "inline-flex shrink-0 items-center gap-1 rounded-md px-2 py-0.5 text-[10px] font-medium text-white transition-colors",
                      "bg-purple-600 hover:bg-purple-700",
                      "disabled:cursor-not-allowed disabled:opacity-50",
                    )}
                    title="Promote this memory to the shared knowledge base"
                  >
                    {isPromoting ? (
                      <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                    ) : (
                      <ArrowUpToLine className="h-3 w-3" aria-hidden />
                    )}
                    {isPromoting ? "Promoting…" : "Promote to Ko"}
                  </button>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </SectionShell>
  );
}

// --- Shared Knowledge (emerald) ---

// const KNOWLEDGE_TONE = {
//   border: "border-emerald-900/50",
//   bg: "bg-gradient-to-br from-emerald-950/55 via-[#0c1814] to-[#0b1220]",
//   text: "text-emerald-200/95",
//   badge: "border border-emerald-500/30 bg-emerald-500/10 text-emerald-200",
// };

// function SharedKnowledgeSection({
//   datapackName,
//   refreshTick,
// }: {
//   datapackName: string | null;
//   refreshTick: number;
// }) {
//   const [items, setItems] = useState<KnowledgeBaseDocument[]>([]);
//   const [loading, setLoading] = useState(false);
//   const [error, setError] = useState<string | null>(null);

//   const fetchDocs = useCallback(
//     async (signal?: AbortSignal) => {
//       if (!datapackName) {
//         setItems([]);
//         return;
//       }
//       setLoading(true);
//       setError(null);
//       try {
//         const list = await getKnowledgeBaseDocuments(datapackName, { signal });
//         setItems(list ?? []);
//       } catch (err) {
//         if ((err as { name?: string })?.name === "CanceledError") return;
//         setError(getApiErrorMessage(err, "Failed to load knowledge sources."));
//         setItems([]);
//       } finally {
//         setLoading(false);
//       }
//     },
//     [datapackName],
//   );

//   useEffect(() => {
//     const ac = new AbortController();
//     void fetchDocs(ac.signal);
//     return () => ac.abort();
//   }, [fetchDocs, refreshTick]);

//   const top = items.slice(0, TOP_N);

//   return (
//     <SectionShell
//       title="Shared knowledge"
//       icon={<BookOpen className="h-3.5 w-3.5 text-emerald-400" aria-hidden />}
//       toneText={KNOWLEDGE_TONE.text}
//       toneBadge={KNOWLEDGE_TONE.badge}
//       count={datapackName ? items.length : undefined}
//       loading={loading}
//       error={error}
//       onRefresh={() => void fetchDocs()}
//     >
//       {!datapackName ? (
//         <p className="text-xs text-muted-foreground">Select a datapack to see knowledge.</p>
//       ) : loading && top.length === 0 ? (
//         <p className="text-xs text-muted-foreground">Loading sources…</p>
//       ) : top.length === 0 ? (
//         <p className="text-xs text-muted-foreground">No knowledge sources yet.</p>
//       ) : (
//         <ul className="space-y-2">
//           {top.map((d) => {
//             const filename = d.document_name.split("/").pop() ?? d.document_name;
//             const display = filename.replace(/^\d+_/, "");
//             return (
//               <li
//                 // Stable key: drop the index suffix so a reorder/insert
//                 // doesn't churn keys for existing tiles (which would
//                 // remount + re-animate every poll).
//                 key={d.document_name}
//                 className={cn(
//                   "flex items-center gap-2 rounded-lg border px-3 py-2 animate-in fade-in slide-in-from-bottom-1 duration-300",
//                   KNOWLEDGE_TONE.border,
//                   KNOWLEDGE_TONE.bg,
//                 )}
//                 title={d.document_name}
//                 tabIndex={0}
//               >
//                 <FileText className="h-3.5 w-3.5 shrink-0 text-emerald-400" aria-hidden />
//                 <span className="min-w-0 flex-1 truncate text-xs font-medium text-white/95">
//                   {display}
//                 </span>
//                 <span
//                   className="shrink-0 text-[10px] uppercase tracking-wide text-emerald-200/70"
//                   aria-label={`status ${d.status}`}
//                 >
//                   {d.status}
//                 </span>
//               </li>
//             );
//           })}
//         </ul>
//       )}
//     </SectionShell>
//   );
// }

// --- Collective Memory (sky: distinct from blue/memories + purple/learnings
// + emerald/knowledge so each section has its own accent). Pulls memories
// that have already been promoted into the tenant knowledge base. ---

const COLLECTIVE_TONE = {
  border: "border-sky-500/40",
  bg: "bg-gradient-to-br from-sky-950/65 via-[#0e1c2a] to-[#0b1220]",
  text: "text-sky-200/95",
  badge: "border border-sky-500/30 bg-sky-500/10 text-sky-200",
};

function CollectiveMemorySection({
  datapackId,
  refreshTick,
  chatTurnOccurred,
}: {
  datapackId: string | null;
  refreshTick: number;
  /** Lets us swap the empty-state hint once a chat turn has fired —
   *  promotion needs memories upstream, so a "still extracting" line
   *  reads better than the static "Promote a learning" copy. */
  chatTurnOccurred: boolean;
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
      icon={<Layers className="h-3.5 w-3.5 text-sky-300" aria-hidden />}
      toneText={COLLECTIVE_TONE.text}
      toneBadge={COLLECTIVE_TONE.badge}
      count={datapackId ? Math.min(items.length, total) : undefined}
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
          <p className="inline-flex items-center gap-2 text-xs text-sky-200/80">
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
              <p className="line-clamp-2 text-xs text-white/95">{m.data}</p>
              {m.agent_id ? (
                <p className="mt-1 truncate text-[10px] text-sky-200/70">
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
