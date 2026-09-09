import type { ReactNode } from "react";

/** Camelcase, UI-side shape both the static Claude reference
 *  (anthropicReferenceReview.ts) and a live run's wire event
 *  (ReviewStreamEvent's context_stats, snake_case) normalize into before
 *  reaching this component. */
export type NormalizedContextStats = {
  kbChunksRetrieved: number;
  kbChunksTotal: number | null;
  kbTokensEstimated: number;
  memoriesRetrieved: number;
  memoriesTotal: number | null;
  memoryTokensEstimated: number;
};

function fraction(retrieved: number, total: number | null): number | null {
  if (total == null || total <= 0) return null;
  return retrieved / total;
}

function ContextBarRow({ label, value, frac }: { label: ReactNode; value: string; frac: number | null }) {
  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-baseline justify-between gap-3 text-xs text-muted-foreground">
        <span>{label}</span>
        <span className="flex-none font-mono text-foreground">{value}</span>
      </div>
      {frac !== null && (
        <div className="h-1.5 overflow-hidden rounded-full bg-muted">
          <div
            className="h-full rounded-full bg-violet-500"
            style={{ width: `${Math.min(100, Math.max(2, frac * 100))}%` }}
          />
        </div>
      )}
    </div>
  );
}

/** Real chunk/memory retrieved-vs-total counts as percentage bars -- the
 *  fraction is exact (both numbers come from a real datapack_describe call
 *  in review.py's _build_context_stats), the token figures are an estimate
 *  (Bedrock exposes no pre-call tokenizer, so each blob's token count is
 *  proportional to its character share of the prompt, anchored to the real
 *  captured usage.input_tokens -- not exact, but not fabricated either).
 *  Shared by AnthropicReferenceCard (the static reference) and
 *  PrReviewDemoApp (a live run), both of which report the same shape. */
export function ContextStatsCard({ stats }: { stats: NormalizedContextStats }) {
  const hasKb = stats.kbChunksRetrieved > 0;
  const hasMemory = stats.memoriesRetrieved > 0;
  if (!hasKb && !hasMemory) return null;

  return (
    <div className="flex flex-col gap-3 rounded-md border border-violet-500/30 bg-violet-500/5 p-3">
      <p className="text-[11px] font-semibold uppercase tracking-wide text-violet-700 dark:text-violet-300">
        What reached the model
      </p>
      {hasKb && (
        <ContextBarRow
          label={
            <>
              <code>knowledgebase_search</code> — {stats.kbChunksRetrieved}
              {stats.kbChunksTotal != null ? ` of ${stats.kbChunksTotal}` : ""} chunks
            </>
          }
          value={`~${stats.kbTokensEstimated} tokens`}
          frac={fraction(stats.kbChunksRetrieved, stats.kbChunksTotal)}
        />
      )}
      {hasMemory && (
        <ContextBarRow
          label={
            <>
              <code>memory_search</code> — {stats.memoriesRetrieved}
              {stats.memoriesTotal != null ? ` of ${stats.memoriesTotal}` : ""} memories
            </>
          }
          value={`~${stats.memoryTokensEstimated} tokens`}
          frac={fraction(stats.memoriesRetrieved, stats.memoriesTotal)}
        />
      )}
      <p className="text-[11px] text-muted-foreground">
        Chunk/memory counts are exact; token figures are estimated (Bedrock exposes no
        pre-call tokenizer).
      </p>
    </div>
  );
}
