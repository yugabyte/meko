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
import {
  getApiErrorMessage,
  getDatapackConversation,
  type DatapackConversationDetail,
  type DatapackThreadMessage,
} from "@/lib/mekoApi";
import { ConversationThreadHeader } from "./ConversationThreadHeader";
import { ThreadTurn } from "./ThreadTurn";
import { useNowMs } from "./useRelativeTime";

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
  // extracted that memory, scroll to it, and shimmer the bubble.
  const [searchParams] = useSearchParams();
  const targetMemoryId = searchParams.get("memoryId") ?? "";
  const msgElRefs = useRef<Record<string, HTMLDivElement | null>>({});
  const didAutoSelectRef = useRef(false);
  const [shimmeringMessageId, setShimmeringMessageId] = useState("");

  useEffect(() => {
    if (!targetMemoryId || !messages.length || didAutoSelectRef.current) return;
    const target = messages.find((m) => m.memory_ids?.includes(targetMemoryId));
    if (!target) return;
    didAutoSelectRef.current = true;
    // Slight delay so the DOM has settled after the thread renders.
    setTimeout(() => {
      msgElRefs.current[target.message_id]?.scrollIntoView({
        behavior: "smooth",
        block: "center",
      });
      setShimmeringMessageId(target.message_id);
      setTimeout(() => setShimmeringMessageId(""), 1800);
    }, 150);
  }, [messages, targetMemoryId]);

  // Reset auto-select flag when conversation changes — otherwise switching
  // conversations would skip the deep-link scroll on the new thread.
  useEffect(() => {
    didAutoSelectRef.current = false;
  }, [conversationId]);

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
      <div className="m-6 flex items-start gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-3 text-sm text-red-300">
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

        {messages.map((m) => (
          <div
            key={m.message_id}
            ref={(el) => {
              msgElRefs.current[m.message_id] = el;
            }}
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
        ))}
      </div>
    </div>
  );
}
