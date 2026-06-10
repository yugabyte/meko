// LiveDecisionTrace — the agentic "Decision trace" block for a freshly
// sent (live) assistant turn in WorkbenchLM.
//
// Historical turns (resumed from a previous conversation) already carry
// their full trace inline from getDatapackConversation, so they render
// <DecisionTrace> directly. A live turn only knows its message_id (from
// POST /chat); Langfuse assembles the rich trace a moment later.
//
// This block reads the per-message trace via the shared
// `useMessageDecisionTrace` query, enabled only once the user expands.
// Because ChatBubble already subscribes to the SAME query key to fill the
// footer chips (eagerly), expanding is normally a cache hit — no second
// network call. If the chips' fetch is still in flight, react-query
// dedupes and this observer just rides along.

import { useState } from "react";
import { Brain, ChevronDown, ChevronRight, Loader2 } from "lucide-react";
import { DecisionTrace } from "@/components/decision-trace-viewer";
import type { DatapackMessageDecisionTrace, DatapackThreadMessage } from "@/lib/mekoApi";
import { useMessageDecisionTrace } from "./useMessageDecisionTrace";

type Props = {
  datapackName: string;
  datapackId?: string;
  conversationId: string;
  /** message_id from the POST /chat response for this assistant turn. */
  messageId: string;
};

export function LiveDecisionTrace({ datapackName, datapackId, conversationId, messageId }: Props) {
  const [expanded, setExpanded] = useState(false);
  const { data, isFetching, isError } = useMessageDecisionTrace(
    { datapackName, datapackId, conversationId, messageId },
    { enabled: expanded },
  );

  const trace = data ? toThreadMessage(data.decision_trace) : null;

  // Data in hand → hand off entirely to the shared DecisionTrace (open by
  // default so the expand click reveals content immediately). It owns its
  // own collapse from here and self-hides if the turn has nothing to show.
  if (expanded && trace) {
    return (
      <DecisionTrace
        message={trace}
        conversationId={conversationId}
        langfuseProjectId={data?.langfuse_project_id}
        datapackName={datapackName}
        datapackId={datapackId}
        enableConversationLink
        defaultExpanded
      />
    );
  }

  // Pre-load header — mirrors DecisionTrace's own collapsed affordance so
  // the swap on load reads as the same control expanding.
  return (
    <div className="mb-2 rounded-lg border border-purple-500/25 bg-purple-500/[0.04]">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-1.5 text-left transition-colors hover:bg-purple-500/[0.08]"
        aria-expanded={expanded}
      >
        {isFetching ? (
          <Loader2 className="h-3 w-3 shrink-0 animate-spin text-purple-600 dark:text-purple-300" aria-hidden />
        ) : expanded ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        )}
        <Brain className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        <span className="text-[11px] font-medium uppercase tracking-wide text-purple-700 dark:text-purple-200/90">
          Decision trace
        </span>
        {isFetching ? (
          <span className="text-[11px] text-muted-foreground">· loading…</span>
        ) : isError ? (
          <span className="text-[11px] text-amber-600 dark:text-amber-400/90">· couldn't load — click to retry</span>
        ) : null}
      </button>
    </div>
  );
}

/** Adapt the slim single-message payload into the `DatapackThreadMessage`
 *  shape `DecisionTrace` consumes. Fields DecisionTrace never reads
 *  (observation_id / started_at / input / output) get inert defaults. */
function toThreadMessage(dt: DatapackMessageDecisionTrace): DatapackThreadMessage {
  return {
    message_id: dt.message_id,
    observation_id: "",
    started_at: "",
    input: "",
    output: "",
    reasoning: dt.reasoning,
    latency_ms: dt.latency_ms,
    memories_extracted: dt.memories_extracted,
    llm_calls: dt.llm_calls,
    total_tokens: dt.total_tokens,
    model: dt.model,
    plan: dt.plan,
    memory_hits: dt.memory_hits,
    kb_hits: dt.kb_hits,
    processing_steps: dt.processing_steps,
  };
}
