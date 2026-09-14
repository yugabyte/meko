import { Sparkles } from "lucide-react";

import {
  ANTHROPIC_REFERENCE_CONTEXT_STATS,
  ANTHROPIC_REFERENCE_FINDINGS,
  ANTHROPIC_REFERENCE_INPUT_TOKENS,
  ANTHROPIC_REFERENCE_IS_POPULATED,
  ANTHROPIC_REFERENCE_MODEL_LABEL,
  ANTHROPIC_REFERENCE_OUTPUT_TOKENS,
} from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { ContextStatsCard } from "./ContextStatsCard";
import { FindingsList } from "./FindingsList";

/** Static reference review from Claude, shown before the user runs their own
 *  review with a smaller model (see ReviewAgentConfigPanel's Model select) --
 *  a fixed comparison point that doesn't depend on a deployed agent being
 *  reachable. Once ANTHROPIC_REFERENCE_IS_POPULATED is real, this becomes a
 *  real, one-time-captured model output (see anthropicReferenceReview.ts),
 *  not fabricated or live -- until then, it isn't rendered at all (see the
 *  guard below), so an unpopulated fixture can't be mistaken for a real "no
 *  issues found" result. The violet accent is this stage's
 *  "reference/benchmark" color, distinct from the orange action/primary
 *  accent used for Run review. */
export function AnthropicReferenceCard() {
  if (!ANTHROPIC_REFERENCE_IS_POPULATED) {
    return (
      <div className="rounded-md border border-dashed border-violet-500/30 bg-violet-500/5 p-3 text-xs text-muted-foreground">
        <Sparkles className="mr-1.5 inline-block h-3.5 w-3.5 text-violet-600 dark:text-violet-300" aria-hidden />
        The {ANTHROPIC_REFERENCE_MODEL_LABEL} reference review hasn't been captured for this lab yet
        It'll show here for comparison once it has.
      </div>
    );
  }
  return (
    <div className="flex flex-col gap-3">
      <div className="overflow-hidden rounded-md border border-violet-500/30">
        <div className="flex items-center gap-2 bg-violet-500/10 px-3 py-2">
          <Sparkles className="h-3.5 w-3.5 text-violet-600 dark:text-violet-300" />
          <span className="text-xs font-medium text-violet-700 dark:text-violet-300">
            Reference: what {ANTHROPIC_REFERENCE_MODEL_LABEL} found, grounded in your org's coding
            standards
          </span>
        </div>
        <div className="flex flex-col gap-2 bg-card p-3">
          <FindingsList findings={ANTHROPIC_REFERENCE_FINDINGS} />
          <p className="border-t border-border pt-2 text-xs text-muted-foreground">
            Reviewed by {ANTHROPIC_REFERENCE_MODEL_LABEL} · {ANTHROPIC_REFERENCE_INPUT_TOKENS} in /{" "}
            {ANTHROPIC_REFERENCE_OUTPUT_TOKENS} out tokens
          </p>
        </div>
      </div>
      <ContextStatsCard
        stats={{
          kbChunksRetrieved: ANTHROPIC_REFERENCE_CONTEXT_STATS.kbChunksRetrieved,
          kbChunksTotal: ANTHROPIC_REFERENCE_CONTEXT_STATS.kbChunksTotal,
          kbTokensEstimated: ANTHROPIC_REFERENCE_CONTEXT_STATS.kbTokensEstimated,
          memoriesRetrieved: ANTHROPIC_REFERENCE_CONTEXT_STATS.memoriesRetrieved,
          memoriesTotal: ANTHROPIC_REFERENCE_CONTEXT_STATS.memoriesTotal,
          memoryTokensEstimated: ANTHROPIC_REFERENCE_CONTEXT_STATS.memoryTokensEstimated,
        }}
      />
    </div>
  );
}
