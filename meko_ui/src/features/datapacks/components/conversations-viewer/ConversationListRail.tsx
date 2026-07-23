// Left rail of the conversation viewer: paginated list of Langfuse
// sessions with click-to-select + infinite scroll on the bottom
// sentinel.
//
// Selection is fully URL-driven — clicking a row navigates to
// `/datapacks/:datapack_id/conversations/:conversationId` so refreshes,
// back/forward, and shared links all land on the same conversation.
//
// Pagination is offset-based against GET /datapacks/:datapack_id/conversations.
// `has_more` from the server tells us when to stop loading; we
// concatenate pages into a single accumulating list rather than
// paging in place (matches Slack/Linear-style channel rails).

import { useCallback, useEffect, useRef, useState } from "react";
import {
  AlertCircle,
  Loader2,
  MessageSquare,
  Search,
  User,
} from "lucide-react";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  listDatapackConversations,
  type DatapackConversation,
} from "@/lib/api/mekoApi";
import { MemoryIcon } from "@/components/conversation-telemetry";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";

/** Page size for the infinite-scroll. Same as the API default; small
 *  enough to keep first paint snappy, big enough that most users won't
 *  see a second fetch before they pick a conversation. */
const PAGE_SIZE = 20;

type Props = {
  datapackId: string;
  /** Currently selected conversation id (URL-derived). Empty when no
   *  selection — the rail still renders, just without a highlight. */
  selectedId: string;
  onSelect: (conversationId: string) => void;
  /** Called once per fresh fetch with the first page so the parent
   *  can auto-select the newest conversation when no `:conversationId`
   *  was provided in the URL. */
  onFirstPageLoaded?: (conversations: DatapackConversation[]) => void;
  /** Reports the `user_email_id` of the currently selected conversation
   *  (or `undefined` when not selected / not yet loaded). The detail
   *  endpoint behind the thread pane doesn't return the author email, so
   *  the parent forwards this rail-sourced value to the thread header. */
  onSelectedEmailChange?: (email: string | undefined) => void;
  className?: string;
};

export function ConversationListRail({
  datapackId,
  selectedId,
  onSelect,
  onFirstPageLoaded,
  onSelectedEmailChange,
  className,
}: Props) {
  const [conversations, setConversations] = useState<DatapackConversation[]>([]);
  const [offset, setOffset] = useState(0);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [debouncedSearch, setDebouncedSearch] = useState("");
  useEffect(() => {
    const timer = setTimeout(() => {
      setDebouncedSearch(search);
    }, 300);
    return () => clearTimeout(timer);
  }, [search]);
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
          datapackId,
          { limit: PAGE_SIZE, offset: nextOffset, q: debouncedSearch.trim() || undefined },
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
    [datapackId, offset, onFirstPageLoaded, debouncedSearch],
  );

  // Reset + first fetch whenever the datapack OR the search filter
  // changes. Search is part of the dep set so a typed query restarts
  // the infinite-scroll cleanly (otherwise we'd append filtered pages
  // onto the unfiltered list).
  useEffect(() => {
    const ac = new AbortController();
    setConversations([]);
    setOffset(0);
    setHasMore(false);
    setFirstLoadDone(false);
    void loadPage({ reset: true, signal: ac.signal });
    return () => ac.abort();
    // loadPage intentionally not in deps — its captured `offset` is
    // stale on first run and we only want this on datapack/search change.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [datapackId, debouncedSearch]);

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

  // Surface the selected conversation's author email to the parent so the
  // thread header can show it — the detail endpoint behind the pane doesn't
  // return `user_email_id`, but the list rows do. Reports `undefined` until
  // the selected row is in the loaded pages (click path or recent deep link).
  const selectedEmail = conversations.find((c) => c.id === selectedId)?.user_email_id;
  useEffect(() => {
    onSelectedEmailChange?.(selectedEmail);
  }, [selectedEmail, onSelectedEmailChange]);

  const showEmpty = firstLoadDone && conversations.length === 0 && !error;

  return (
    <aside
      className={cn(
        // `bg-card` (not `bg-input`) so the rail reads as the same
        // white panel surface as the `<ConversationThreadHeader>` on
        // the right — visually they're one continuous "viewer" panel.
        // The thread CONTENT below the header still sits on the page
        // background, letting message bubbles float against it.
        "flex h-full min-h-0 flex-col border-r border-border bg-card",
        className,
      )}
    >
      <div className="flex items-center gap-2 px-4 py-3 border-b border-border shrink-0">
        <MessageSquare className="h-4 w-4 text-foreground" aria-hidden />
        <span className="text-sm font-medium text-foreground">Conversations</span>
        {firstLoadDone ? (
          <span className="ml-auto text-[11px] text-muted-foreground tabular-nums">
            {conversations.length}
            {hasMore ? "+" : ""}
          </span>
        ) : null}
      </div>
      <div className="relative shrink-0 border-b border-border px-3 py-2">
        <Search
          className="pointer-events-none absolute left-5 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
          aria-hidden
        />
        <Input
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search conversations…"
          className="h-8 border border-border bg-background pl-8 text-xs"
          aria-label="Search conversations"
        />
      </div>

      <div className="flex-1 min-h-0 overflow-y-auto [scrollbar-color:hsl(var(--meko-navy-700))_hsl(var(--meko-navy-900))] [scrollbar-width:thin] [&::-webkit-scrollbar]:w-2 [&::-webkit-scrollbar-track]:bg-input [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-meko-navy-700 hover:[&::-webkit-scrollbar-thumb]:bg-meko-navy-600">
        {error ? (
          <div className="m-3 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-3 py-2 text-xs text-red-700 dark:text-red-300">
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
                    "block w-full cursor-pointer border-b border-border/60 px-4 py-3 text-left transition-colors",
                    "border-l-2",
                    active
                      ? "bg-input border-l-brand"
                      : "border-l-transparent hover:bg-input",
                  )}
                  // Announce the conversation by its headline so screen-reader
                  // users hear what the row is about, not just the visible
                  // message/memory counts. Cap at 80 chars so long agent
                  // payloads don't drown out the action verb.
                  aria-label={
                    c.title || c.first_message
                      ? `Open conversation: ${headline.length > 80
                        ? `${headline.slice(0, 80)}…`
                        : headline
                      }`
                      : `Open conversation ${c.id.slice(0, 8)}…`
                  }
                >
                  <div className="flex items-center justify-between gap-2 mb-1">
                    {c.agent_id ? (
                      <span className="inline-flex items-center gap-1 truncate text-[10px] text-blue-700 dark:text-blue-300">
                        <span className="truncate">{c.agent_id}</span>
                      </span>
                    ) : (
                      <span className="text-[10px] text-muted-foreground/40">—</span>
                    )}
                    <span className="shrink-0 text-[10px] text-muted-foreground whitespace-nowrap">
                      {formatRelativeTime(c.last_activity_at, now)}
                    </span>
                  </div>
                  {/* Headline is clamped to two lines. `line-clamp-2` already
                      handles the ellipsis on the last visible line; we add
                      `break-words` so a single very long token (no spaces)
                      doesn't silently disable the ellipsis, and `text-ellipsis`
                      explicitly forces the dots even when the browser is
                      borderline about whether overflow exists. The full text
                      is always available on hover via `title`. */}
                  <p
                    className={cn(
                      "mb-2 line-clamp-2 break-words text-ellipsis text-[13px] leading-snug",
                      // Both active + inactive go through `text-foreground`
                      // so it themes — `text-white` for the active row
                      // collapsed to invisible white-on-light-grey in
                      // light mode. The active row already stands out via
                      // its `bg-input` fill + `border-l-brand` rail.
                      active ? "text-foreground" : "text-foreground/85",
                    )}
                    title={headline}
                  >
                    {headline}
                  </p>
                  <div className="flex items-center gap-2.5 text-[10px] text-muted-foreground">
                    <span className="tabular-nums">{(c.messages ?? 0).toLocaleString()} msgs</span>
                    {(c.memories_added ?? 0) > 0 ? (
                      <>
                        <span>·</span>
                        <span className="inline-flex items-center gap-1 text-blue-700 dark:text-blue-300">
                          <MemoryIcon className="text-[10px]" />
                          {c.memories_added.toLocaleString()}
                        </span>
                      </>
                    ) : null}
                    {c.user_email_id ? (
                      // Author of the conversation, right-aligned. Truncates
                      // (with full value on hover) so long emails don't push
                      // the msg/memory counts around in the narrow rail.
                      <span
                        className="ml-auto inline-flex min-w-0 items-center gap-1 text-muted-foreground"
                        title={c.user_email_id}
                      >
                        <User className="h-2.5 w-2.5 shrink-0" aria-hidden />
                        <span className="truncate">{c.user_email_id}</span>
                      </span>
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
