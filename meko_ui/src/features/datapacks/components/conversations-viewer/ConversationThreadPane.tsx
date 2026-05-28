// Right pane of the conversation viewer: orchestrates the conversation
// fetch + paginated thread state, then composes the layout from focused
// sub-components:
//
//   - `<ConversationThreadHeader>`  — title, chips, action buttons
//   - `<ThreadTurn>`                — one user+assistant bubble pair per turn
//   - `<MemoryHoverCard>`           — the memories-extracted hover popover
//                                     (rendered inside `<ThreadTurn>`)
//
// Pagination is offset-based against
// `GET /datapacks/:name/conversations/:conversationId`. We render the
// first page on mount; when `thread_has_more` is true a "Load older
// messages" button appears at the top of the scroll area.
//
// Deep-link: when navigated to with `?memoryId=<id>`, we find the message
// that extracted that memory, scroll it into view, and shimmer the bubble
// briefly so the user can spot which turn produced their target memory.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { AlertCircle, Loader2, MessageSquare } from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  getDatapackConversation,
  type DatapackConversationDetail,
  type DatapackThreadMessage,
} from "@/lib/mekoApi";
import { ConversationThreadHeader } from "./ConversationThreadHeader";
import { ThreadTurn } from "./ThreadTurn";
import { useNowMs } from "@/lib/relativeTime";

/** Initial per-page thread size. Most production conversations sit
 *  well under 100 turns; older history is loaded on demand. */
const THREAD_PAGE_SIZE = 100;

type Props = {
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  /** Called when the user clicks "View trace" on an assistant turn —
   *  parent owns the drawer state so the trace panel can persist
   *  across re-renders. */
  onOpenTrace: (messageId: string) => void;
  /** Variant of `onOpenTrace` that also briefly shimmers the source
   *  bubble — used by the memoryId deep-link flow so the user can spot
   *  which turn produced their target memory. */
  onOpenTraceWithMemoryShimmer?: (messageId: string) => void;
};

export function ConversationThreadPane({
  datapackName,
  datapackId,
  conversationId,
  onOpenTrace,
  onOpenTraceWithMemoryShimmer,
}: Props) {
  const [detail, setDetail] = useState<DatapackConversationDetail | null>(null);
  const [langfuseProjectId, setLangfuseProjectId] = useState<string>("");
  /** Older pages are prepended to this array in load order. The rendered
   *  thread = [...olderPages, ...detail.thread]. */
  const [olderMessages, setOlderMessages] = useState<DatapackThreadMessage[]>([]);
  const [olderOffset, setOlderOffset] = useState(0);
  const [olderHasMore, setOlderHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expandedReasoning, setExpandedReasoning] = useState<Record<string, boolean>>({});

  const now = useNowMs();

  // Reset + fetch first thread page whenever the selected conversation changes.
  useEffect(() => {
    if (!conversationId) {
      setDetail(null);
      setLangfuseProjectId("");
      setOlderMessages([]);
      setOlderOffset(0);
      setOlderHasMore(false);
      setError(null);
      setExpandedReasoning({});
      return;
    }
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    setDetail(null);
    setOlderMessages([]);
    setExpandedReasoning({});
    getDatapackConversation(
      datapackName,
      conversationId,
      { messagesLimit: THREAD_PAGE_SIZE, messagesOffset: 0 },
      { signal: ac.signal, datapackId },
    )
      .then((resp) => {
        if (ac.signal.aborted) return;
        setDetail(resp.conversation);
        setLangfuseProjectId(resp.langfuse_project_id ?? "");
        setOlderOffset(resp.conversation.thread.length);
        setOlderHasMore(resp.conversation.thread_has_more);
      })
      .catch((err) => {
        if (ac.signal.aborted) return;
        setError(getApiErrorMessage(err, "Failed to load conversation."));
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [datapackName, conversationId, datapackId]);

  const loadOlder = useCallback(async () => {
    if (!conversationId || !olderHasMore || loadingOlder) return;
    setLoadingOlder(true);
    try {
      const resp = await getDatapackConversation(
        datapackName,
        conversationId,
        { messagesLimit: THREAD_PAGE_SIZE, messagesOffset: olderOffset },
        { datapackId },
      );
      const incoming = resp.conversation.thread ?? [];
      setOlderMessages((prev) => [...prev, ...incoming]);
      setOlderOffset(olderOffset + incoming.length);
      setOlderHasMore(resp.conversation.thread_has_more);
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to load older messages."));
    } finally {
      setLoadingOlder(false);
    }
  }, [datapackName, conversationId, olderOffset, olderHasMore, loadingOlder, datapackId]);

  const toggleReasoning = useCallback((id: string) => {
    setExpandedReasoning((prev) => ({ ...prev, [id]: !prev[id] }));
  }, []);

  /** Concatenated message list. Older pages first (chronological ordering),
   *  then the initial page. */
  const messages = useMemo(() => {
    if (!detail) return [] as DatapackThreadMessage[];
    return [...detail.thread, ...olderMessages];
  }, [detail, olderMessages]);

  // Deep-link: when arriving with ?memoryId=<id>, find the message that
  // extracted that memory, scroll to it, shimmer the turn, and open the
  // trace drawer on the extracted-memories panel.
  const [searchParams] = useSearchParams();
  const targetMemoryId = searchParams.get("memoryId") ?? "";
  const msgElRefs = useRef<Record<string, HTMLDivElement | null>>({});
  const didDeepLinkRef = useRef(false);
  const [shimmeringMessageId, setShimmeringMessageId] = useState("");
  const [deepLinkPending, setDeepLinkPending] = useState(false);

  useEffect(() => {
    didDeepLinkRef.current = false;
    setDeepLinkPending(!!targetMemoryId);
    setShimmeringMessageId("");
  }, [conversationId, targetMemoryId]);

  const revealDeepLinkTarget = useCallback(
    (messageId: string) => {
      if (didDeepLinkRef.current) return;
      didDeepLinkRef.current = true;
      setDeepLinkPending(false);
      window.setTimeout(() => {
        msgElRefs.current[messageId]?.scrollIntoView({
          behavior: "smooth",
          block: "center",
        });
        setShimmeringMessageId(messageId);
        onOpenTraceWithMemoryShimmer?.(messageId);
        window.setTimeout(() => setShimmeringMessageId(""), 2800);
      }, 150);
    },
    [onOpenTraceWithMemoryShimmer],
  );

  useEffect(() => {
    if (!deepLinkPending || !targetMemoryId || loading) return;
    const target = messages.find((m) => m.memory_ids?.includes(targetMemoryId));
    if (target) {
      revealDeepLinkTarget(target.message_id);
      return;
    }
    if (olderHasMore && !loadingOlder) {
      void loadOlder();
      return;
    }
    if (!olderHasMore) {
      setDeepLinkPending(false);
    }
  }, [
    deepLinkPending,
    targetMemoryId,
    messages,
    loading,
    olderHasMore,
    loadingOlder,
    loadOlder,
    revealDeepLinkTarget,
  ]);

  if (loading) {
    return (
      <div className="flex h-full items-center justify-center text-sm text-muted-foreground">
        <Loader2 className="mr-2 h-4 w-4 animate-spin" aria-hidden />
        Loading conversation…
      </div>
    );
  }

  if (error) {
    return (
      <div className="m-6 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-700 dark:text-red-300">
        <AlertCircle className="h-4 w-4 shrink-0 mt-0.5" aria-hidden />
        <span>{error}</span>
      </div>
    );
  }

  if (!detail) {
    return (
      <div className="flex h-full flex-col items-center justify-center text-sm text-muted-foreground">
        <MessageSquare className="mb-2 h-6 w-6 opacity-50" aria-hidden />
        Pick a conversation from the rail to inspect.
      </div>
    );
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <ConversationThreadHeader
        detail={detail}
        conversationId={conversationId}
        langfuseProjectId={langfuseProjectId}
        now={now}
      />

      <style>{`
        @keyframes meko-deeplink-pulse {
          0%, 100% { box-shadow: 0 0 0 2px rgb(251 191 36 / 0.55), 0 0 16px 0 rgb(251 191 36 / 0.25); }
          50%      { box-shadow: 0 0 0 2px rgb(251 191 36 / 0.85), 0 0 24px 4px rgb(251 191 36 / 0.35); }
        }
      `}</style>
      <div className="flex-1 min-h-0 overflow-y-auto px-6 py-6 space-y-6">
        {olderHasMore ? (
          <div className="flex justify-center">
            <Button
              variant="outline"
              size="sm"
              className="border-border bg-background/80 text-muted-foreground hover:text-foreground"
              onClick={() => void loadOlder()}
              disabled={loadingOlder}
            >
              {loadingOlder ? (
                <Loader2 className="mr-2 h-3.5 w-3.5 animate-spin" aria-hidden />
              ) : null}
              Load older messages
            </Button>
          </div>
        ) : null}

        {messages.length === 0 ? (
          <p className="text-center text-sm text-muted-foreground">
            This conversation has no messages yet.
          </p>
        ) : null}

        {messages.map((m) => {
          const isTarget = shimmeringMessageId === m.message_id;
          return (
          <div
            key={m.message_id}
            ref={(el) => {
              msgElRefs.current[m.message_id] = el;
            }}
            data-deeplink-target={isTarget ? "true" : undefined}
            // Pulsing amber halo signals "this is the turn that produced
            // the memory you clicked on". Scroll margin keeps it clear of
            // the sticky thread header when `scrollIntoView` centers it.
            // Background + padding are baked in so the entrance is purely
            // additive (no layout shift on highlight start/end).
            className={cn(
              "scroll-mt-24 rounded-xl px-3 py-2 -mx-3 transition-colors duration-300",
              isTarget && "bg-amber-500/[0.04] dark:bg-amber-400/[0.06]",
            )}
            style={
              isTarget
                ? { animation: "meko-deeplink-pulse 1.4s ease-in-out 2" }
                : undefined
            }
          >
            <ThreadTurn
              message={m}
              now={now}
              reasoningExpanded={!!expandedReasoning[m.message_id]}
              onToggleReasoning={() => toggleReasoning(m.message_id)}
              onOpenTrace={() => onOpenTrace(m.message_id)}
              onOpenTraceWithMemoryShimmer={
                onOpenTraceWithMemoryShimmer
                  ? () => onOpenTraceWithMemoryShimmer(m.message_id)
                  : undefined
              }
              isShimmering={shimmeringMessageId === m.message_id}
              datapackName={datapackName}
              datapackId={datapackId}
              conversationId={conversationId}
            />
          </div>
          );
        })}
      </div>
    </div>
  );
}
