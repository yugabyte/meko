// Left rail of the conversation viewer: paginated list of Langfuse
// sessions with click-to-select + infinite scroll on the bottom
// sentinel.
//
// Selection is fully URL-driven — clicking a row navigates to
// `/datapacks/:name/conversations/:conversationId` so refreshes,
// back/forward, and shared links all land on the same conversation.
//
// Pagination is offset-based against GET /datapacks/:name/conversations.
// `has_more` from the server tells us when to stop loading; we
// concatenate pages into a single accumulating list rather than
// paging in place (matches Slack/Linear-style channel rails).

import { useCallback, useEffect, useRef, useState } from "react";
import {
  AlertCircle,
  Bot,
  Loader2,
  MessageSquare,
  Sparkles,
} from "lucide-react";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  listDatapackConversations,
  type DatapackConversation,
} from "@/lib/mekoApi";
import { formatRelativeTime, useNowMs } from "./useRelativeTime";

/** Page size for the infinite-scroll. Same as the API default; small
 *  enough to keep first paint snappy, big enough that most users won't
 *  see a second fetch before they pick a conversation. */
const PAGE_SIZE = 20;

type Props = {
  datapackName: string;
  /** Currently selected conversation id (URL-derived). Empty when no
   *  selection — the rail still renders, just without a highlight. */
  selectedId: string;
  onSelect: (conversationId: string) => void;
  /** Called once per fresh fetch with the first page so the parent
   *  can auto-select the newest conversation when no `:conversationId`
   *  was provided in the URL. */
  onFirstPageLoaded?: (conversations: DatapackConversation[]) => void;
  className?: string;
};

export function ConversationListRail({
  datapackName,
  selectedId,
  onSelect,
  onFirstPageLoaded,
  className,
}: Props) {
  const [conversations, setConversations] = useState<DatapackConversation[]>([]);
  const [offset, setOffset] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** True once the very first fetch resolves — drives the empty
   *  state vs initial-loader split. */
  const [firstLoadDone, setFirstLoadDone] = useState(false);

  const now = useNowMs();
  const sentinelRef = useRef<HTMLDivElement | null>(null);

  /** Load one page. Pages are appended unless `reset=true`, which
   *  also resets offset back to 0 (used on datapack switch). */
  const loadPage = useCallback(
    async (params: { reset?: boolean; signal?: AbortSignal }) => {
      const reset = params.reset ?? false;
      setLoading(true);
      setError(null);
      try {
        const nextOffset = reset ? 0 : offset;
        const resp = await listDatapackConversations(
          datapackName,
          { limit: PAGE_SIZE, offset: nextOffset },
          { signal: params.signal },
        );
        if (params.signal?.aborted) return;
        const incoming = resp.conversations ?? [];
        setConversations((prev) => (reset ? incoming : [...prev, ...incoming]));
        setOffset(nextOffset + incoming.length);
        setHasMore(resp.has_more);
        if (reset && onFirstPageLoaded) onFirstPageLoaded(incoming);
        setFirstLoadDone(true);
      } catch (err) {
        if (params.signal?.aborted) return;
        setError(getApiErrorMessage(err, "Failed to load conversations."));
      } finally {
        if (!params.signal?.aborted) setLoading(false);
      }
    },
    [datapackName, offset, onFirstPageLoaded],
  );

  // Reset + first fetch whenever the datapack changes.
  useEffect(() => {
    const ac = new AbortController();
    setConversations([]);
    setOffset(0);
    setHasMore(false);
    setFirstLoadDone(false);
    void loadPage({ reset: true, signal: ac.signal });
    return () => ac.abort();
    // loadPage intentionally not in deps — its captured `offset` is
    // stale on first run and we only want this on datapack change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [datapackName]);

  // Bottom sentinel observer — triggers next page when scrolled into
  // view. We re-create the observer when `hasMore`/`loading` flip so
  // that we don't fire requests when there's nothing left or one is
  // already in flight.
  useEffect(() => {
    const node = sentinelRef.current;
    if (!node || !hasMore || loading) return;
    const io = new IntersectionObserver(
      (entries) => {
        const visible = entries[0]?.isIntersecting ?? false;
        if (visible) void loadPage({});
      },
      { rootMargin: "120px" },
    );
    io.observe(node);
    return () => io.disconnect();
  }, [hasMore, loading, loadPage]);

  const showEmpty = firstLoadDone && conversations.length === 0 && !error;

  return (
    <aside
      className={cn(
        "flex h-full min-h-0 flex-col border-r border-border bg-[#0E1525]",
        className,
      )}
    >
      <div className="flex items-center gap-2 px-4 py-3 border-b border-border shrink-0">
        <MessageSquare className="h-4 w-4 text-purple-300" aria-hidden />
        <span className="text-sm font-medium text-white">Conversations</span>
        {firstLoadDone ? (
          <span className="ml-auto text-[11px] text-muted-foreground tabular-nums">
            {conversations.length}
            {hasMore ? "+" : ""}
          </span>
        ) : null}
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto [scrollbar-color:#2B3550_#0E1525] [scrollbar-width:thin] [&::-webkit-scrollbar]:w-2 [&::-webkit-scrollbar-track]:bg-[#0E1525] [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-[#2B3550] hover:[&::-webkit-scrollbar-thumb]:bg-[#3A4A70]">
        {error ? (
          <div className="m-3 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-300">
            <AlertCircle className="h-4 w-4 shrink-0 mt-0.5" aria-hidden />
            <span>{error}</span>
          </div>
        ) : null}

        {showEmpty ? (
          <p className="px-4 py-8 text-center text-xs text-muted-foreground">
            No conversations yet for this datapack.
          </p>
        ) : null}

        <ul>
          {conversations.map((c) => {
            const active = c.id === selectedId;
            const headline = c.title || c.first_message || "(no messages yet)";
            return (
              <li key={c.id}>
                <button
                  type="button"
                  onClick={() => onSelect(c.id)}
                  className={cn(
                    "block w-full border-b border-border/60 px-4 py-3 text-left transition-colors",
                    "border-l-2",
                    active
                      ? "bg-[#141C2E] border-l-purple-400"
                      : "border-l-transparent hover:bg-[#11192A]",
                  )}
                >
                  <div className="flex items-center justify-between gap-2 mb-1">
                    {c.agent_id ? (
                      <span className="inline-flex items-center gap-1 truncate text-[10px] text-blue-300">
                        <Bot className="h-2.5 w-2.5 shrink-0" aria-hidden />
                        <span className="truncate">{c.agent_id}</span>
                      </span>
                    ) : (
                      <span className="text-[10px] text-muted-foreground/40">—</span>
                    )}
                    <span className="shrink-0 text-[10px] text-muted-foreground whitespace-nowrap">
                      {formatRelativeTime(c.last_activity_at, now)}
                    </span>
                  </div>
                  <p
                    className={cn(
                      "mb-2 line-clamp-2 text-[13px] leading-snug",
                      active ? "text-white" : "text-foreground/85",
                    )}
                  >
                    {headline}
                  </p>
                  <div className="flex items-center gap-2.5 text-[10px] text-muted-foreground">
                    <span className="tabular-nums">{c.messages.toLocaleString()} msgs</span>
                    {c.memories_added > 0 ? (
                      <>
                        <span>·</span>
                        <span className="inline-flex items-center gap-1 text-blue-300">
                          <Sparkles className="h-2.5 w-2.5" aria-hidden />
                          {c.memories_added.toLocaleString()}
                        </span>
                      </>
                    ) : null}
                  </div>
                </button>
              </li>
            );
          })}
        </ul>

        {/* Sentinel + spinner: visible when there's a next page to
            fetch. The empty div is what the IntersectionObserver
            actually watches; the spinner is purely cosmetic. */}
        {hasMore || loading ? (
          <div ref={sentinelRef} className="flex items-center justify-center py-4">
            {loading ? (
              <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" aria-hidden />
            ) : (
              <span className="text-[10px] text-muted-foreground">Scroll for more…</span>
            )}
          </div>
        ) : null}
      </div>
    </aside>
  );
}
