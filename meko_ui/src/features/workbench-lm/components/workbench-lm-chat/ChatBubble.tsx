// One bubble in the chat transcript — user (right, blue) or assistant
// (left, dark). Assistant bubbles render markdown and a meta row with
// Copy / latency / memories / llm / View trace.

import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import { DecisionTrace } from "@/components/decision-trace-viewer";
import type { DatapackThreadMessage } from "@/lib/api/mekoApi";
import { AssistantMetaRow } from "./AssistantMetaRow";
import { LiveDecisionTrace } from "./LiveDecisionTrace";
import { useMessageDecisionTrace } from "./useMessageDecisionTrace";
import type { Message } from "./types";

export function ChatBubble({
  message,
  onOpenTrace,
  traceMessage,
  conversationId,
  langfuseProjectId,
  datapackName,
  datapackId,
}: {
  message: Message;
  onOpenTrace?: () => void;
  /** When this assistant turn was loaded from a previous conversation,
   *  the raw `DatapackThreadMessage` (matched by message_id) is passed
   *  in so we render the full `DecisionTrace` straight from the already-
   *  fetched thread data. Undefined for live turns. */
  traceMessage?: DatapackThreadMessage;
  /** Active conversation id — used by the historical DecisionTrace's
   *  deep-links and as the lookup key for the live trace fetch. */
  conversationId?: string;
  /** Langfuse project id for the resumed conversation (historical turns
   *  only — the live trace fetch returns its own). */
  langfuseProjectId?: string;
  /** Live-fetch context: present for live turns so the bubble can lazily
   *  pull this message's decision trace on expand. */
  datapackName?: string | null;
  datapackId?: string | null;
}) {
  const isUser = message.role === "user";
  // Live assistant turns (not loaded from history) fetch their decision
  // trace by message_id. The trace block (LiveDecisionTrace) reads it
  // lazily on expand; this eager subscription fills the footer chips.
  // Both share one react-query cache entry, so they coalesce into a
  // single network call — see useMessageDecisionTrace.
  const canFetchLiveTrace =
    !traceMessage &&
    message.role === "assistant" &&
    !!message.langfuseMessageId &&
    !!conversationId &&
    !!datapackName;

  // Always called (rules of hooks); `enabled` gates the actual fetch so
  // user bubbles and historical turns never hit the network here.
  const liveTrace = useMessageDecisionTrace(
    {
      datapackName,
      datapackId,
      conversationId,
      messageId: message.langfuseMessageId ?? null,
    },
    { enabled: canFetchLiveTrace },
  );
  const liveDt = liveTrace.data?.decision_trace;

  // Chip values: historical turns carry them on the synthesised Message;
  // live turns read them off the eager query (undefined until it lands).
  const latencyMs = message.latencyMs ?? liveDt?.latency_ms;
  const memoriesExtracted = message.memoriesExtracted ?? liveDt?.memories_extracted;
  const llmCalls = message.llmCalls ?? liveDt?.llm_calls;

  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[85%] rounded-2xl px-4 py-2.5 text-sm leading-relaxed",
          isUser ? "bg-blue-600/90 text-white" : "border border-border bg-card text-foreground",
        )}
      >
        {isUser ? (
          <div className="whitespace-pre-wrap break-words">{message.content}</div>
        ) : (
          <>
            {/*
              Agentic thinking ("Decision trace") sits ABOVE the answer
              per spec. Both paths use the SAME DecisionTrace component so
              live and historical turns read identically; each self-hides
              when there's nothing to show.

              - Historical turns (resumed from a previous conversation):
                the full trace is already in `traceMessage` (from
                getDatapackConversation) → render DecisionTrace directly.
              - Live turns (just sent via POST /chat): only the message_id
                is known up front; LiveDecisionTrace lazily fetches the
                trace by that id when the user expands the block.
            */}
            {traceMessage ? (
              <DecisionTrace
                message={traceMessage}
                conversationId={conversationId ?? ""}
                langfuseProjectId={langfuseProjectId}
                datapackName={datapackName ?? undefined}
                datapackId={datapackId ?? undefined}
                enableConversationLink
              />
            ) : canFetchLiveTrace ? (
              <LiveDecisionTrace
                datapackName={datapackName!}
                datapackId={datapackId ?? undefined}
                conversationId={conversationId!}
                messageId={message.langfuseMessageId!}
              />
            ) : null}
            <MarkdownMessage text={message.content} />
          </>
        )}
        {!isUser && (
          <AssistantMetaRow
            text={message.content}
            onOpenTrace={onOpenTrace}
            latencyMs={latencyMs}
            memoriesExtracted={memoriesExtracted}
            llmCalls={llmCalls}
          />
        )}
      </div>
    </div>
  );
}

/** Spinner placeholder shown while a turn is in flight. */
export function ThinkingBubble() {
  return (
    <div className="flex justify-start">
      <div className="flex max-w-[85%] items-center gap-2 rounded-2xl border border-border bg-card px-4 py-2.5 text-sm text-muted-foreground">
        <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
        Thinking…
      </div>
    </div>
  );
}
