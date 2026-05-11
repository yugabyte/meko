// One user prompt + one assistant response, rendered as a bubble pair.
//
// The thread API returns each turn as a single ThreadMessage carrying both
// `input` (user) and `output` (assistant); reasoning, when present, sits
// between the avatar and the answer as a collapsible block.
//
// Per-turn telemetry (latency / memories extracted / llm calls) lives in
// the footer below the assistant bubble, with the memories badge wrapped
// in `<MemoryHoverCard>` so the user can peek the extracted texts without
// opening the trace drawer.

import { Activity, Bot, Brain, ChevronDown, ChevronUp, Clock, Cpu, User } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import { formatLatencyLabel } from "@/components/conversation-telemetry";
import type { DatapackThreadMessage } from "@/lib/mekoApi";
import { MemoryHoverCard } from "./MemoryHoverCard";
import { formatRelativeTime } from "./useRelativeTime";
import { normalizeMarkdownText } from "./markdownNormalize";

export type ThreadTurnProps = {
  message: DatapackThreadMessage;
  now: number;
  reasoningExpanded: boolean;
  onToggleReasoning: () => void;
  onOpenTrace: () => void;
  /** Variant of `onOpenTrace` that also shimmers this bubble briefly when
   *  the drawer opens — used for the memoryId deep-link flow so the user
   *  can spot where in the thread their target memory was extracted. */
  onOpenTraceWithMemoryShimmer?: () => void;
  isShimmering?: boolean;
  datapackName: string;
  datapackId?: string;
  conversationId: string;
};

export function ThreadTurn({
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
              View decision trace
            </button>
          </div>
        </div>
      </div>
    </div>
  );
}
