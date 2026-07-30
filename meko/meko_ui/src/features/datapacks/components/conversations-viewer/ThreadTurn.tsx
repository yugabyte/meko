// One user prompt + one assistant response, rendered as a bubble pair.
//
// The thread API returns each turn as a single ThreadMessage carrying both
// `input` (user) and `output` (assistant); reasoning now renders inside
// the Decision Trace panel when present.
//
// Per-turn telemetry (latency / memories extracted / llm calls) lives in
// the footer below the assistant bubble, with the memories badge wrapped
// in `<MemoryHoverCard>` so the user can peek the extracted texts without
// opening the trace drawer.

import { Activity, Bot, Clock, Cpu, User } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import { formatLatencyLabel } from "@/components/conversation-telemetry";
import type { DatapackThreadMessage } from "@/lib/api/mekoApi";
import { DecisionTrace } from "@/components/decision-trace-viewer";
import { MemoryHoverCard } from "./MemoryHoverCard";
import { formatRelativeTime } from "@/lib/relativeTime";
import { normalizeMarkdownText } from "./markdownNormalize";

export type ThreadTurnProps = {
  message: DatapackThreadMessage;
  now: number;
  onOpenTrace: () => void;
  /** Variant of `onOpenTrace` that also shimmers this bubble briefly when
   *  the drawer opens — used for the memoryId deep-link flow so the user
   *  can spot where in the thread their target memory was extracted. */
  onOpenTraceWithMemoryShimmer?: () => void;
  /** Opens the trace drawer focused on a specific span — fired by
   *  "View Trace" on a Decision Trace step. */
  onViewTraceSpan?: (spanId: string) => void;
  isShimmering?: boolean;
  datapackId: string;
  /** Display-only: lets Decision Trace render a `datapack_id` PARAMS row as
   *  the datapack name. Routing/API always use `datapackId`. */
  datapackName?: string;
  /** Email of the conversation's author — rendered in place of the raw
   *  `user_id` UUID in Decision Trace PARAMS. */
  userEmail?: string;
  conversationId: string;
  /** Required to build the deep-link footer / "Open in Langfuse" anchors
   *  inside the per-turn Decision Trace card. Empty string is treated
   *  the same as missing (the card hides the link rather than rendering
   *  a dead button). */
  langfuseProjectId?: string;
};

export function ThreadTurn({
  message,
  now,
  onOpenTrace,
  onOpenTraceWithMemoryShimmer,
  onViewTraceSpan,
  isShimmering = false,
  datapackId,
  datapackName,
  userEmail,
  conversationId,
  langfuseProjectId,
}: ThreadTurnProps) {
  const normalizedOutput = normalizeMarkdownText(message.output);
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
          <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border bg-muted">
            <User className="h-3.5 w-3.5 text-muted-foreground" aria-hidden />
          </div>
        </div>
      ) : null}

      {/* Assistant bubble — left-aligned. Always rendered (even when
          output is empty) so the per-turn stats stay attached to the
          turn the user is looking at. */}
      <div className="flex max-w-3xl gap-3">
        <div className="flex h-7 w-7 shrink-0 items-center justify-center rounded-md border border-border bg-card">
          <Bot className="h-3.5 w-3.5 text-foreground/80" aria-hidden />
        </div>
        <div className="min-w-0 flex-1">
          <div className="mb-1 font-mono text-[10px] text-muted-foreground">{relTime}</div>

          {/* datapackName/datapackId are passed so a `datapack_id` PARAMS
              row renders as the datapack name. enableConversationLink is
              intentionally OFF: this view already IS the conversation, so
              deep-linking a step's conversation_id back to the same page
              is redundant (that link is WorkbenchLM-only). */}
          <DecisionTrace
            message={message}
            conversationId={conversationId}
            langfuseProjectId={langfuseProjectId}
            datapackName={datapackName}
            datapackId={datapackId}
            userEmail={userEmail}
            onViewTrace={onViewTraceSpan}
          />

          <div className="relative overflow-hidden rounded-2xl">
            {hasOutput ? (
              <div
                className={cn(
                  "rounded-2xl rounded-tl-sm border px-4 py-3 text-sm leading-relaxed text-foreground/95 whitespace-pre-wrap transition-colors duration-300",
                  isShimmering ? "border-foreground/20 bg-muted" : "border-border bg-card",
                )}
              >
                <MarkdownMessage text={normalizedOutput} />
              </div>
            ) : (
              <div className="rounded-2xl rounded-tl-sm border border-dashed border-border bg-card/50 px-4 py-3 text-sm italic text-muted-foreground">
                (no assistant response captured for this turn)
              </div>
            )}
            {isShimmering ? (
              <div
                aria-hidden
                className="pointer-events-none absolute inset-0 z-10"
                style={{
                  animation: "memory-shimmer 1.6s ease-in-out forwards",
                  background:
                    "linear-gradient(105deg, transparent 30%, rgba(255,255,255,0.18) 50%, rgba(255,255,255,0.08) 58%, transparent 70%)",
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
            {(message.memories_extracted ?? 0) > 0 ||
            (message.promoted_memory_ids?.length ?? 0) > 0 ? (
              <MemoryHoverCard
                count={message.memories_extracted ?? 0}
                memories={message.memory_texts ?? []}
                promotedCount={message.promoted_memory_ids?.length ?? 0}
                promotedMemories={message.promoted_memory_texts ?? []}
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
              className="inline-flex cursor-pointer items-center gap-1 rounded px-2 py-0.5 text-purple-700 dark:text-purple-300 hover:bg-card hover:text-purple-800 dark:hover:text-purple-200 transition-colors"
              aria-label="View execution trace"
              title="Open the Langfuse execution trace for this message"
            >
              <Activity className="h-3 w-3" aria-hidden />
              View execution trace
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
