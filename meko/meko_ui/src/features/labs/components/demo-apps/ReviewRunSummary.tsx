// What Chapter 3's first lesson shows once the review lands: how long it
// took, how many findings came back, the pipeline that produced them, and a
// green banner quoting the run's own input tokens.
//
// Deliberately NOT the findings themselves. Lesson 2 is the read of those,
// and rendering the same list twice made the first lesson look like the
// second one with extra chrome.

import { Brain, FileDiff, ListChecks, Search, Sparkles } from "lucide-react";

import { WarningBanner } from "@/components/ui/warning-banner";

import type { ReviewFinding } from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { BehindTheScenes, type PipelineStep } from "../stages/BehindTheScenes";
import type { ContextStats } from "./PrReviewDemoApp";

function formatSeconds(ms: number): string {
  return `${(ms / 1000).toFixed(1)}s`;
}

export function ReviewRunSummary({
  findings,
  usage,
  stats,
  durationMs,
  modelLabel,
  enabledTools,
}: {
  /** "unstructured" when the agent answered in prose, which is a different
   *  thing from finding nothing, so no count is claimed for it. */
  findings: ReviewFinding[] | "unstructured";
  usage: { input_tokens: number; output_tokens: number } | null;
  stats: ContextStats | null;
  /** Wall clock for the run, measured here rather than reported by the
   *  agent, so it includes the round trip the user actually waited through. */
  durationMs: number | null;
  modelLabel: string;
  enabledTools: string[];
}) {
  const structured = Array.isArray(findings) ? findings : null;
  const critical = structured?.filter((f) => f.severity === "critical").length ?? null;
  const major = structured?.filter((f) => f.severity === "major").length ?? null;

  const steps: PipelineStep[] = [
    {
      icon: FileDiff,
      label: "Diff sent",
      detail: "The pull request, with no rules attached",
    },
    ...(enabledTools.includes("knowledgebase_search")
      ? [
          {
            icon: Search,
            label: "knowledgebase_search",
            detail:
              stats && stats.kb_chunks_total != null
                ? `${stats.kb_chunks_retrieved} of ${stats.kb_chunks_total} chunks matched`
                : stats
                  ? `${stats.kb_chunks_retrieved} chunks matched`
                  : "Queried your knowledge base",
          } satisfies PipelineStep,
        ]
      : []),
    ...(enabledTools.includes("memory_search")
      ? [
          {
            icon: Brain,
            label: "memory_search",
            detail: stats
              ? `${stats.memories_retrieved} memories matched`
              : "Queried your memories",
          } satisfies PipelineStep,
        ]
      : []),
    {
      icon: Sparkles,
      label: modelLabel,
      detail: usage
        ? `${usage.input_tokens.toLocaleString()} in, ${usage.output_tokens.toLocaleString()} out`
        : "Ran the review",
    },
    {
      icon: ListChecks,
      label: "Findings parsed",
      detail:
        structured === null
          ? "The agent answered in prose"
          : `${structured.length} comment${structured.length === 1 ? "" : "s"}, each with a severity`,
    },
  ];

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-x-2 gap-y-1 font-mono text-xs text-muted-foreground">
        {durationMs !== null && (
          <span className="text-context-knowledge">{formatSeconds(durationMs)}</span>
        )}
        {structured !== null ? (
          <>
            <span>
              {structured.length} finding{structured.length === 1 ? "" : "s"}
            </span>
            <span aria-hidden>·</span>
            <span className={critical ? "text-destructive" : undefined}>{critical} critical</span>
            <span aria-hidden>·</span>
            <span className={major ? "text-warning" : undefined}>{major} major</span>
          </>
        ) : (
          <span>the agent answered in prose, so nothing was counted</span>
        )}
      </div>

      <BehindTheScenes concept="knowledge" steps={steps} />

      {usage && (
        <WarningBanner tone="success">
          <b className="font-semibold">{usage.input_tokens.toLocaleString()} input tokens.</b>{" "}
          {stats && stats.kb_chunks_retrieved > 0 && stats.kb_chunks_total != null
            ? `It asked for ${stats.kb_chunks_retrieved} of your ${stats.kb_chunks_total} chunks instead of reading all of them`
            : "It asked the datapack for what this diff needed instead of carrying the whole document"}
          {structured && structured.length > 0
            ? ", and found what the cold run missed."
            : "."}{" "}
          Read the findings next.
        </WarningBanner>
      )}
    </div>
  );
}
