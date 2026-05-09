// Right pane of the conversation viewer: header strip with the
// session-level chips followed by an alternating user / assistant
// bubble stream.
//
// Each assistant turn carries a collapsible "agent reasoning" block
// (rendered only when the API returned a non-empty `reasoning` from
// the matched Reasoning generation), the per-turn stats footer
// (latency, memories extracted, llm calls), and a "View
// trace" button that opens the slide-over TraceDrawer for that
// message id.
//
// Pagination is offset-based against
// GET /datapacks/:name/conversations/:conversationId. We start with
// the first thread page (newest by API contract is "oldest first" —
// the bookmark spans are inserted in arrival order); when
// `thread_has_more` is true we render a "Load older messages" button
// at the top, mirroring chat clients.

import {
  Children,
  createElement,
  isValidElement,
  memo,
  type ReactElement,
  type ReactNode,
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import { useSearchParams } from "react-router-dom";
import ReactMarkdown, { type Options } from "react-markdown";
import remarkGfm from "remark-gfm";
import {
  Activity,
  AlertCircle,
  Bot,
  Brain,
  ChevronDown,
  ChevronUp,
  Clock,
  Cpu,
  ExternalLink,
  Loader2,
  MessageSquare,
  Send,
  User,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import {
  getApiErrorMessage,
  getDatapackConversation,
  getDatapackMessageTrace,
  type DatapackConversationDetail,
  type DatapackThreadMessage,
} from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";

import {
  MemoryIcon,
  formatLatencyLabel,
} from "@/components/conversation-telemetry";

import { ROUTES } from "@/app/routes/paths";

import { formatRelativeTime, useNowMs } from "./useRelativeTime";
import { parseMemoryOutput } from "./parseMemoryOutput";

/** Initial per-page thread size. Most production conversations sit
 *  well under 100 turns; older history is loaded on demand. */
const THREAD_PAGE_SIZE = 100;
const MARKDOWN_RENDER_CHARACTER_LIMIT = 150000;
const MemoizedReactMarkdown = memo(ReactMarkdown);

const isTextElement = (
  child: ReactNode,
): child is ReactElement<{ className?: string }> =>
  isValidElement(child) &&
  typeof child.type === "string" &&
  ["p", "h1", "h2", "h3", "h4", "h5", "h6"].includes(child.type);

const transformListItemChildren = (children: ReactNode) =>
  Children.map(children, (child) =>
    isTextElement(child)
      ? createElement("span", {
        ...child.props,
        className: cn(child.props.className, "mb-1"),
      })
      : child,
  );

type Props = {
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  /** Called when the user clicks "View trace" on an assistant turn —
   *  parent owns the drawer state so the trace panel can persist
   *  across thread re-fetches. */
  onOpenTrace: (messageId: string) => void;
  /** Like onOpenTrace but also asks TraceDrawer to shimmer the memories
   *  panel — called when the user clicks directly on the memory badge. */
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
  /** Older pages are prepended to this array in load order. The
   *  rendered thread = [...olderPages, ...detail.thread]. */
  const [olderMessages, setOlderMessages] = useState<DatapackThreadMessage[]>([]);
  const [olderOffset, setOlderOffset] = useState(0);
  const [olderHasMore, setOlderHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expandedReasoning, setExpandedReasoning] = useState<Record<string, boolean>>({});

  const now = useNowMs();

  // Reset + fetch first thread page whenever the selected
  // conversation changes.
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
  }, [datapackName, conversationId]);

  const loadOlder = useCallback(async () => {
    if (!conversationId || !olderHasMore || loadingOlder) return;
    setLoadingOlder(true);
    try {
      const resp = await getDatapackConversation(datapackName, conversationId, {
        messagesLimit: THREAD_PAGE_SIZE,
        messagesOffset: olderOffset,
      }, { datapackId });
      const incoming = resp.conversation.thread ?? [];
      setOlderMessages((prev) => [...prev, ...incoming]);
      setOlderOffset(olderOffset + incoming.length);
      setOlderHasMore(resp.conversation.thread_has_more);
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to load older messages."));
    } finally {
      setLoadingOlder(false);
    }
  }, [datapackName, conversationId, olderOffset, olderHasMore, loadingOlder]);

  const toggleReasoning = useCallback((id: string) => {
    setExpandedReasoning((prev) => ({ ...prev, [id]: !prev[id] }));
  }, []);

  /** Concatenated message list. Older pages first (chronological
   *  ordering), then the initial page. */
  const messages = useMemo(() => {
    if (!detail) return [] as DatapackThreadMessage[];
    return [...detail.thread, ...olderMessages];
  }, [detail, olderMessages]);

  // Deep-link: when arriving with ?memoryId=<id>, find the message that
  // extracted that memory, scroll to it, and shimmer that message bubble.
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
    setTimeout(() => {
      msgElRefs.current[target.message_id]?.scrollIntoView({ behavior: "smooth", block: "center" });
      setShimmeringMessageId(target.message_id);
      setTimeout(() => setShimmeringMessageId(""), 1800);
    }, 150);
  }, [messages, targetMemoryId]);

  // Reset auto-select flag when conversation changes.
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

  const headline = detail.title || detail.first_message || "(no messages yet)";
  const startedDisplay = formatDatapackCreatedAtUtc(detail.started_at);

  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="border-b border-border bg-[#11192A] px-6 py-4 shrink-0">
        <div className="flex items-start justify-between gap-4">
          <div className="min-w-0 flex-1">
            <h2 className="truncate text-base font-medium text-white" title={headline}>
              {headline}
            </h2>
            <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
              {detail.agent_id ? (
                <>
                  <span className="inline-flex items-center gap-1">
                    <Bot className="h-3 w-3" aria-hidden />
                    <code className="text-blue-300">{detail.agent_id}</code>
                  </span>
                  <span>·</span>
                </>
              ) : null}
              <span title={detail.started_at}>started {startedDisplay}</span>
              <span>·</span>
              <span>
                last activity {formatRelativeTime(detail.last_activity_at, now)}
              </span>
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-1.5">
              <Chip icon={<MessageSquare className="h-3 w-3" />} accent="slate">
                {(detail.messages ?? 0).toLocaleString()} messages
              </Chip>
              {(detail.memories_added ?? 0) > 0 ? (
                <Chip icon={<MemoryIcon className="text-[14px]" />} accent="blue">
                  {detail.memories_added.toLocaleString()} memories extracted
                </Chip>
              ) : null}
              {(detail.llm_calls ?? 0) > 0 ? (
                <Chip icon={<Cpu className="h-3 w-3" />} accent="amber">
                  {detail.llm_calls.toLocaleString()} llm calls
                </Chip>
              ) : null}
            </div>
          </div>
          <div className="flex items-center gap-2 shrink-0">
            {langfuseProjectId ? (
              <a
                href={`${ROUTES.observeHub}?${new URLSearchParams({ project: langfuseProjectId, session: conversationId }).toString()}`}
                className="inline-flex items-center gap-1 rounded-md border border-border bg-muted/10 px-2 py-1 text-[11px] font-medium text-foreground/80 hover:bg-muted/20 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-purple-500/40 transition-colors"
                title="Open this conversation in the Langfuse trace view"
              >
                <ExternalLink className="h-3 w-3" aria-hidden />
                <span>Observe</span>
              </a>
            ) : null}
            <Button
              variant="outline"
              size="sm"
              className="gap-1.5 border-border bg-background text-muted-foreground hover:text-foreground"
              disabled
              title="Continuing this conversation from the viewer is not wired yet"
            >
              <Send className="h-3.5 w-3.5" aria-hidden />
              Continue
            </Button>
          </div>
        </div>
      </header>

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
            ref={(el) => { msgElRefs.current[m.message_id] = el; }}
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

// ---------------------------------------------------------------------------
// One user prompt + one assistant response, rendered as a bubble pair.
//
// The thread API returns each turn as a single ThreadMessage with both
// `input` (user prompt) and `output` (assistant reply); reasoning, when
// present, sits between them as a collapsible block.
// ---------------------------------------------------------------------------

type ThreadTurnProps = {
  message: DatapackThreadMessage;
  now: number;
  reasoningExpanded: boolean;
  onToggleReasoning: () => void;
  onOpenTrace: () => void;
  onOpenTraceWithMemoryShimmer?: () => void;
  isShimmering?: boolean;
  datapackName: string;
  datapackId?: string;
  conversationId: string;
};

function ThreadTurn({
  message,
  now,
  reasoningExpanded,
  onToggleReasoning,
  onOpenTrace,
  onOpenTraceWithMemoryShimmer,
  isShimmering = false,
  datapackName,
  datapackId,
  conversationId,
}: ThreadTurnProps) {
  const normalizedReasoning = normalizeMarkdownText(message.reasoning);
  const normalizedOutput = normalizeMarkdownText(message.output);
  const hasReasoning = normalizedReasoning.length > 0;
  const hasOutput = normalizedOutput.length > 0;
  const relTime = formatRelativeTime(message.started_at, now);
  const latencyLabel = formatLatencyLabel(message.latency_ms);

  return (
    <div className="space-y-4">
      {isShimmering ? (
        <style>{`
          @keyframes memory-shimmer {
            0%   { transform: translateX(-100%); }
            100% { transform: translateX(100%); }
          }
        `}</style>
      ) : null}

      {/* User bubble — right-aligned. */}
      {message.input ? (
        <div className="flex max-w-3xl ml-auto justify-end gap-3">
          <div className="text-right">
            <div className="mb-1 text-[10px] text-muted-foreground">{relTime}</div>
            <div className="rounded-2xl rounded-br-sm border border-blue-500/30 bg-blue-500/10 px-4 py-2.5 text-left text-sm text-foreground/95 whitespace-pre-wrap">
              {message.input}
            </div>
          </div>
          <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-emerald-500/40 bg-emerald-500/15">
            <User className="h-3.5 w-3.5 text-emerald-300" aria-hidden />
          </div>
        </div>
      ) : null}

      {/* Assistant bubble — left-aligned. Always rendered (even when
          output is empty) so the per-turn stats stay attached to the
          turn the user is looking at. */}
      <div className="flex max-w-3xl gap-3">
        <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border bg-[#1C2333]">
          <Bot className="h-3.5 w-3.5 text-foreground/80" aria-hidden />
        </div>
        <div className="min-w-0 flex-1">
          <div className="mb-1 font-mono text-[10px] text-muted-foreground">{relTime}</div>

          {hasReasoning ? (
            <div className="mb-2">
              <button
                type="button"
                onClick={onToggleReasoning}
                className={cn(
                  "flex w-full items-center gap-2 rounded-md border px-3 py-1.5 text-[11px] transition-colors",
                  reasoningExpanded
                    ? "border-purple-500/30 bg-purple-500/10 text-purple-200"
                    : "border-border bg-[#1C2333] text-muted-foreground hover:border-purple-500/30 hover:text-foreground",
                )}
              >
                <Brain className="h-3 w-3" aria-hidden />
                <span className="flex-1 text-left">Agent reasoning</span>
                {reasoningExpanded ? (
                  <ChevronUp className="h-3 w-3" aria-hidden />
                ) : (
                  <ChevronDown className="h-3 w-3" aria-hidden />
                )}
              </button>
              {reasoningExpanded ? (
                <div className="mt-1 rounded-md border border-purple-500/20 bg-purple-500/5 px-3 py-2.5 text-[12.5px] italic leading-relaxed text-purple-100/90 whitespace-pre-wrap">
                  <MarkdownMessage text={normalizedReasoning} />
                </div>
              ) : null}
            </div>
          ) : null}

          <div className="relative overflow-hidden rounded-2xl">
            {hasOutput ? (
              <div className={cn(
                "rounded-2xl rounded-tl-sm border px-4 py-3 text-sm leading-relaxed text-foreground/95 whitespace-pre-wrap transition-colors duration-300",
                isShimmering ? "border-white/30 bg-[#252F45]" : "border-border bg-[#1C2333]",
              )}>
                <MarkdownMessage text={normalizedOutput} />
              </div>
            ) : (
              <div className="rounded-2xl rounded-tl-sm border border-dashed border-border bg-[#1C2333]/50 px-4 py-3 text-sm italic text-muted-foreground">
                (no assistant response captured for this turn)
              </div>
            )}
            {isShimmering ? (
              <div
                aria-hidden
                className="pointer-events-none absolute inset-0 z-10"
                style={{
                  animation: "memory-shimmer 1.6s ease-in-out forwards",
                  background: "linear-gradient(105deg, transparent 30%, rgba(255,255,255,0.18) 50%, rgba(255,255,255,0.08) 58%, transparent 70%)",
                  width: "200%",
                  height: "100%",
                  top: "0",
                  left: "0",
                }}
              />
            ) : null}
          </div>

          <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 pl-1 text-[11px] text-muted-foreground">
            <span className="inline-flex items-center gap-1">
              <Clock className="h-2.5 w-2.5" aria-hidden />
              {latencyLabel}
            </span>
            {(message.memories_extracted ?? 0) > 0 ? (
              <MemoryHoverCard
                count={message.memories_extracted}
                messageId={message.message_id}
                datapackName={datapackName}
                datapackId={datapackId}
                conversationId={conversationId}
                onClickOpenTrace={onOpenTraceWithMemoryShimmer ?? onOpenTrace}
              />
            ) : null}
            {(message.llm_calls ?? 0) > 0 ? (
              <span className="inline-flex items-center gap-1">
                <Cpu className="h-2.5 w-2.5 text-amber-400" aria-hidden />
                {message.llm_calls.toLocaleString()} llm
              </span>
            ) : null}
            <span className="text-border">|</span>
            <button
              type="button"
              onClick={onOpenTrace}
              className="inline-flex items-center gap-1 rounded px-2 py-0.5 text-purple-300 hover:bg-[#1C2333] hover:text-purple-200 transition-colors"
            >
              <Activity className="h-3 w-3" aria-hidden />
              View trace
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------------------
// MemoryHoverCard: fetches trace spans on hover and surfaces the extracted
// memory texts in a floating popover above the badge. Lazy — no network
// request until the user actually hovers.
// ---------------------------------------------------------------------------

const MEMORY_PREVIEW_CHARS = 120;

type MemoryHoverCardProps = {
  count: number;
  messageId: string;
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  onClickOpenTrace: () => void;
};

function MemoryHoverCard({
  count,
  messageId,
  datapackName,
  datapackId,
  conversationId,
  onClickOpenTrace,
}: MemoryHoverCardProps) {
  const [open, setOpen] = useState(false);
  const [memories, setMemories] = useState<string[] | null>(null);
  const [fetching, setFetching] = useState(false);
  const fetchedRef = useRef(false);
  const closeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const fetchMemories = useCallback(() => {
    if (fetchedRef.current) return;
    fetchedRef.current = true;
    setFetching(true);
    getDatapackMessageTrace(datapackName, conversationId, messageId, {
      datapackId,
    })
      .then((resp) => {
        const spans = resp.spans ?? [];
        const memoryIds = new Set(
          spans.filter((s) => /memor/i.test(s.name) && s.output).map((s) => s.id),
        );
        const topLevel = spans.filter(
          (s) =>
            /memor/i.test(s.name) &&
            s.output &&
            (!s.parent_observation_id || !memoryIds.has(s.parent_observation_id)),
        );
        setMemories(topLevel.flatMap((s) => parseMemoryOutput(s.output ?? "")));
      })
      .catch(() => setMemories([]))
      .finally(() => setFetching(false));
  }, [datapackName, conversationId, messageId, datapackId]);

  const openPopover = useCallback(() => {
    if (closeTimerRef.current) clearTimeout(closeTimerRef.current);
    setOpen(true);
    fetchMemories();
  }, [fetchMemories]);

  const closePopover = useCallback(() => {
    closeTimerRef.current = setTimeout(() => setOpen(false), 150);
  }, []);

  const label = `${count.toLocaleString()} ${count === 1 ? "memory" : "memories"} extracted`;

  return (
    <span className="relative inline-flex items-center">
      {/* Badge */}
      <span
        role="button"
        tabIndex={0}
        className={cn(
          "inline-flex cursor-pointer select-none items-center gap-1 rounded px-1 py-0.5 transition-colors",
          open ? "bg-blue-500/15 text-blue-300" : "hover:bg-blue-500/10 hover:text-blue-300",
        )}
        onMouseEnter={openPopover}
        onMouseLeave={closePopover}
        onClick={onClickOpenTrace}
        onKeyDown={(e) => e.key === "Enter" && onClickOpenTrace()}
      >
        <MemoryIcon className="text-[13px]" />
        {label}
      </span>

      {/* Popover */}
      {open ? (
        <div
          className="absolute bottom-full left-0 z-50 mb-2 w-72 overflow-hidden rounded-xl border border-blue-500/25 bg-[#0E1525] shadow-2xl"
          onMouseEnter={openPopover}
          onMouseLeave={closePopover}
          onClick={onClickOpenTrace}
        >
          {/* Caret */}
          <div className="absolute -bottom-1.5 left-4 h-3 w-3 rotate-45 border-b border-r border-blue-500/25 bg-[#0E1525]" />

          <div className="px-3.5 py-3">
            <p className="mb-2.5 flex items-center gap-1.5 text-[11px] font-medium text-blue-300">
              <MemoryIcon className="text-[13px]" />
              Extracted memories
              <span className="ml-auto text-[10px] font-normal text-blue-300/50">
                Click to open trace
              </span>
            </p>

            {fetching ? (
              <div className="flex items-center gap-2 text-[11px] text-muted-foreground">
                <Loader2 className="h-3 w-3 animate-spin" aria-hidden />
                Loading…
              </div>
            ) : !memories || memories.length === 0 ? (
              <p className="text-[11px] italic text-muted-foreground">
                No memory text captured in trace.
              </p>
            ) : (
              <ul className="space-y-2">
                {memories.map((text, i) => (
                  // eslint-disable-next-line react/no-array-index-key
                  <li key={i} className="flex items-start gap-2">
                    <span className="mt-[5px] h-1.5 w-1.5 shrink-0 rounded-full bg-blue-400/70" />
                    <span className="text-[12px] leading-relaxed text-foreground/85">
                      {text.length > MEMORY_PREVIEW_CHARS
                        ? `${text.slice(0, MEMORY_PREVIEW_CHARS)}…`
                        : text}
                    </span>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </span>
  );
}

// ---------------------------------------------------------------------------
// Small inline pill helper for the header chips. Inlined (rather than
// importing a shared Badge) so the conversation viewer can pick its
// own color palette without polluting global components.
// ---------------------------------------------------------------------------

type ChipProps = {
  icon?: React.ReactNode;
  accent: "slate" | "blue" | "emerald" | "amber" | "purple";
  children: React.ReactNode;
};

function Chip({ icon, accent, children }: ChipProps) {
  const palette: Record<ChipProps["accent"], string> = {
    slate: "bg-[#1C2333] text-foreground/85 border-border",
    blue: "bg-blue-500/10 text-blue-300 border-blue-500/30",
    emerald: "bg-emerald-500/10 text-emerald-300 border-emerald-500/30",
    amber: "bg-amber-500/10 text-amber-300 border-amber-500/30",
    purple: "bg-purple-500/10 text-purple-300 border-purple-500/30",
  };
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1 rounded-full border px-2 py-0.5 text-[11px]",
        palette[accent],
      )}
    >
      {icon}
      {children}
    </span>
  );
}

function normalizeMarkdownText(value?: string): string {
  if (!value) return "";
  const unescaped = value.includes("\\n") ? value.replaceAll("\\n", "\n") : value;
  // Keep ordered list markers attached to content when models emit:
  // "1.\n\n**Title**" or "1.\n\n### Title" instead of "1. ...".
  return unescaped.replace(/(^|\n)(\d+)\.\s*\n+\s*(?=\S)/g, "$1$2. ");
}

function MarkdownMessage({ text }: { text: string }) {
  if (text.length > MARKDOWN_RENDER_CHARACTER_LIMIT) {
    return <p className="whitespace-pre-wrap">{text}</p>;
  }

  return (
    <div className="space-y-2 break-words">
      <MemoizedReactMarkdown
        remarkPlugins={[remarkGfm]}
        components={{
          p: ({ children }) => <p className="mb-2 whitespace-pre-wrap leading-relaxed last:mb-0">{children}</p>,
          ul: ({ children }) => <ul className="mb-2 ml-5 list-inside list-disc">{children}</ul>,
          ol: ({ children }) => <ol className="mb-2 ml-5 list-inside list-decimal">{children}</ol>,
          li: ({ children }) => (
            <li className="mt-1 leading-relaxed [&>ol]:pl-4 [&>p]:m-0 [&>p]:inline [&>ul]:pl-4">
              {transformListItemChildren(children)}
            </li>
          ),
          h1: ({ children }) => <h1 className="mb-2 text-base font-semibold">{children}</h1>,
          h2: ({ children }) => <h2 className="mb-2 text-[15px] font-semibold">{children}</h2>,
          h3: ({ children }) => <h3 className="mb-1.5 text-sm font-semibold">{children}</h3>,
          code: ({ children }) => (
            <code className="rounded bg-black/25 px-1 py-0.5 font-mono text-[0.9em]">{children}</code>
          ),
          pre: ({ children }) => (
            <pre className="mb-2 overflow-x-auto rounded-md bg-black/25 p-3 font-mono text-[12px] [&>code]:bg-transparent [&>code]:p-0">
              {children}
            </pre>
          ),
          a: ({ children, href }) => (
            <a
              href={href}
              target="_blank"
              rel="noreferrer noopener"
              className="text-blue-300 underline decoration-blue-400/60 underline-offset-2 hover:text-blue-200"
            >
              {children}
            </a>
          ),
        }}
      >
        {text}
      </MemoizedReactMarkdown>
    </div>
  );
}
