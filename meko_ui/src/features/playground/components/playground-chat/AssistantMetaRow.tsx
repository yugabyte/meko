// Footer strip under each assistant bubble: Copy button on the left,
// optional telemetry chips (latency, memories extracted, llm calls), and a
// "View trace" deep-link on the right. Mirrors
// `ConversationThreadPane`'s meta row so the two surfaces feel identical.

import { useState } from "react";
import { Activity, Check, Clock, Copy, Cpu } from "lucide-react";
import { toast } from "@/hooks/use-toast";
import {
  MemoryIcon,
  formatLatencyLabel,
} from "@/components/conversation-telemetry";

export function AssistantMetaRow({
  text,
  onOpenTrace,
  latencyMs,
  memoriesExtracted,
  llmCalls,
}: {
  text: string;
  onOpenTrace?: () => void;
  latencyMs?: number;
  memoriesExtracted?: number;
  llmCalls?: number;
}) {
  const [copied, setCopied] = useState(false);

  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      toast({ title: "Copied to clipboard" });
      // Revert the icon after a moment so the user gets visual confirmation
      // even though the toast also confirms.
      setTimeout(() => setCopied(false), 1500);
    } catch {
      toast({
        title: "Couldn't copy",
        description: "Browser blocked clipboard access.",
      });
    }
  };

  // Each chip is independently optional — the enrichment fetch may land
  // partial data (e.g. `memories_extracted` lags Mem0's background extractor
  // by 30–60s). Missing pieces simply don't render rather than placeholdering.
  const hasLatency = typeof latencyMs === "number" && Number.isFinite(latencyMs);
  const hasMemories = (memoriesExtracted ?? 0) > 0;
  const hasLlmCalls = (llmCalls ?? 0) > 0;
  const hasAnyMeta = hasLatency || hasMemories || hasLlmCalls;

  return (
    <div className="mt-2 flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border/50 pt-2 text-[11px] text-muted-foreground">
      <button
        type="button"
        onClick={handleCopy}
        className="inline-flex items-center gap-1 transition-colors hover:text-foreground"
        aria-label="Copy"
      >
        {copied ? (
          <Check className="h-3 w-3 text-emerald-400" aria-hidden />
        ) : (
          <Copy className="h-3 w-3" aria-hidden />
        )}
        {copied ? "Copied" : "Copy"}
      </button>
      {hasAnyMeta ? <span className="text-border">|</span> : null}
      {hasLatency ? (
        <span className="inline-flex items-center gap-1">
          <Clock className="h-2.5 w-2.5" aria-hidden />
          {formatLatencyLabel(latencyMs!)}
        </span>
      ) : null}
      {hasMemories ? (
        <span className="inline-flex items-center gap-1">
          <MemoryIcon className="text-[13px]" />
          {memoriesExtracted!.toLocaleString()}{" "}
          {memoriesExtracted === 1 ? "memory" : "memories"} extracted
        </span>
      ) : null}
      {hasLlmCalls ? (
        <span className="inline-flex items-center gap-1">
          <Cpu className="h-2.5 w-2.5 text-amber-400" aria-hidden />
          {llmCalls!.toLocaleString()} llm
        </span>
      ) : null}
      {onOpenTrace ? (
        <>
          <span className="text-border">|</span>
          <button
            type="button"
            onClick={onOpenTrace}
            className="inline-flex items-center gap-1 rounded px-2 py-0.5 text-purple-300 hover:bg-[#1C2333] hover:text-purple-200 transition-colors"
            aria-label="View trace"
            title="Open the Langfuse trace for this message"
          >
            <Activity className="h-3 w-3" aria-hidden />
            View trace
          </button>
        </>
      ) : null}
    </div>
  );
}
