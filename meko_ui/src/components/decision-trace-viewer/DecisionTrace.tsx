// DecisionTrace — per-turn card showing agentic thinking, retrieval,
// and per-turn metadata sourced from Langfuse.
//
// Top-level orchestrator only. Each visual region is a sibling file:
//   - DecisionTraceHeader.tsx — chip totals + trace_id
//   - PhaseAccordion.tsx      — accordion shell + accent palette
//   - PlanContent.tsx         — Plan-phase split-source body
//   - DecisionTraceStep.tsx   — one row inside a phase body
// Pure helpers live in helpers.ts.

import { useMemo, useState } from "react";
import {
  Brain,
  ChevronDown,
  ChevronRight,
  Layers,
  Sparkles,
  Workflow,
} from "lucide-react";
import type { DatapackThreadMessage } from "@/lib/mekoApi";
import { normalizeMarkdownText } from "@/features/datapacks/components/conversations-viewer/markdownNormalize";
import {
  DecisionTraceHeader,
  type DbBreakdownEntry,
} from "./DecisionTraceHeader";
import { PhaseAccordion } from "./PhaseAccordion";
import { PlanContent } from "./PlanContent";
import {
  derivePlanAndReasoning,
  executeTagline,
  groupStepsByPhase,
  insertSyntheticContextAssemble,
  planTagline,
  prettifyStepName,
  preprocessTagline,
} from "./helpers";

export type DecisionTraceProps = {
  message: DatapackThreadMessage;
  conversationId: string;
  langfuseProjectId?: string;
  /** Initial expanded state. Defaults to collapsed. Set true when the
   *  card is mounted as a result of the user already having clicked to
   *  expand (e.g. WorkbenchLM's lazy live-trace fetch) so the content
   *  shows immediately instead of requiring a second click. */
  defaultExpanded?: boolean;
};

export function DecisionTrace({
  message,
  conversationId,
  langfuseProjectId,
  defaultExpanded = false,
}: DecisionTraceProps) {
  const [expanded, setExpanded] = useState(defaultExpanded);

  const reasoningRaw = normalizeMarkdownText(message.reasoning).trim();
  const { reasoningText, planSteps } = useMemo(
    () => derivePlanAndReasoning(reasoningRaw, message.plan),
    [reasoningRaw, message.plan],
  );
  const hasReasoning = reasoningText.length > 0;
  const hasPlan = planSteps.length > 0;
  const memoryHits = message.memory_hits ?? [];
  const kbHits = message.kb_hits ?? [];
  const rawSteps = message.processing_steps ?? [];

  const stepsWithContext = useMemo(
    () => insertSyntheticContextAssemble(rawSteps, memoryHits, kbHits),
    [rawSteps, memoryHits, kbHits],
  );
  const phases = useMemo(
    () => groupStepsByPhase(stepsWithContext),
    [stepsWithContext],
  );

  // Header `db` chip counts every step badge-labelled `DB` by
  // `stepKindLabel`: memory.search / knowledge.retrieve (reads) plus
  // memory.add (write). conversation.persist is excluded because it's
  // badge-labelled `LOG` — it writes a Langfuse audit row, not a
  // datapack DB row. Keeping the two in sync prevents a turn that has
  // a visible DB-badged step from showing `db: 0` in the header.
  const dbBreakdown = useMemo<DbBreakdownEntry[]>(() => {
    const order = ["memory_search", "knowledgebase_search", "memory_add"];
    const acc = new Map<string, { count: number; ms: number }>();
    for (const s of rawSteps) {
      if (!order.includes(s.name)) continue;
      const cur = acc.get(s.name) ?? { count: 0, ms: 0 };
      cur.count += 1;
      cur.ms += s.duration_ms ?? 0;
      acc.set(s.name, cur);
    }
    return order
      .filter((n) => acc.has(n))
      .map((n) => ({
        name: n,
        prettyName: prettifyStepName(n),
        count: acc.get(n)!.count,
        ms: acc.get(n)!.ms,
      }));
  }, [rawSteps]);
  const dbCalls = useMemo(
    () => dbBreakdown.reduce((a, b) => a + b.count, 0),
    [dbBreakdown],
  );

  const hasAnythingToShow =
    rawSteps.length > 0 ||
    memoryHits.length > 0 ||
    kbHits.length > 0 ||
    hasReasoning ||
    hasPlan;
  if (!hasAnythingToShow) return null;

  const summaryParts: string[] = [];
  if (rawSteps.length > 0) summaryParts.push(`${rawSteps.length} step${rawSteps.length === 1 ? "" : "s"}`);
  if (memoryHits.length > 0) summaryParts.push(`${memoryHits.length} mem`);
  if (kbHits.length > 0) summaryParts.push(`${kbHits.length} chunks`);
  if (hasPlan) summaryParts.push(`${planSteps.length} planned`);
  else if (hasReasoning) summaryParts.push("reasoning");
  const summary = summaryParts.join(" · ");

  return (
    <div className="mb-2 rounded-lg border border-purple-500/25 bg-purple-500/[0.04]">
      <button
        type="button"
        onClick={() => setExpanded((e) => !e)}
        className="flex w-full cursor-pointer items-center gap-2 rounded-lg px-3 py-1.5 text-left transition-colors hover:bg-purple-500/[0.08]"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        ) : (
          <ChevronRight className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        )}
        <Brain className="h-3 w-3 shrink-0 text-purple-600 dark:text-purple-300" aria-hidden />
        <span className="text-[11px] font-medium uppercase tracking-wide text-purple-700 dark:text-purple-200/90">
          Decision trace
        </span>
        {summary ? (
          <span className="text-[11px] text-muted-foreground">· {summary}</span>
        ) : null}
      </button>

      {expanded ? (
        <div className="space-y-2 border-t border-purple-500/15 px-3 py-3">
          <DecisionTraceHeader
            totalMs={message.latency_ms}
            totalTokens={message.total_tokens ?? 0}
            dbCalls={dbCalls}
            dbBreakdown={dbBreakdown}
            llmCalls={message.llm_calls ?? 0}
            conversationId={conversationId}
            langfuseProjectId={langfuseProjectId}
          />

          <PhaseAccordion
            number={1}
            label="Preprocess"
            accent="blue"
            icon={<Layers className="h-3 w-3 text-blue-600 dark:text-blue-300" aria-hidden />}
            steps={phases.preprocess}
            taglineFallback={preprocessTagline(phases.preprocess, memoryHits, kbHits)}
            conversationId={conversationId}
            langfuseProjectId={langfuseProjectId}
          />

          <PhaseAccordion
            number={2}
            label="Plan"
            accent="purple"
            icon={<Sparkles className="h-3 w-3 text-purple-600 dark:text-purple-300" aria-hidden />}
            steps={[]}
            taglineFallback={planTagline(planSteps, hasReasoning)}
            conversationId={conversationId}
            langfuseProjectId={langfuseProjectId}
            hasContentOverride={hasReasoning || hasPlan}
          >
            <PlanContent
              reasoning={reasoningText}
              plan={planSteps}
              model={message.model}
              accent="purple"
            />
          </PhaseAccordion>

          <PhaseAccordion
            number={3}
            label="Execute"
            accent="emerald"
            icon={<Workflow className="h-3 w-3 text-emerald-600 dark:text-emerald-300" aria-hidden />}
            steps={phases.execute}
            taglineFallback={executeTagline(phases.execute, message.memories_extracted ?? 0)}
            conversationId={conversationId}
            langfuseProjectId={langfuseProjectId}
          />
        </div>
      ) : null}
    </div>
  );
}
