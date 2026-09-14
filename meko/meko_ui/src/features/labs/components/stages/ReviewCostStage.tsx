// Chapter 3's payoff: what the review the user just ran actually cost, and
// what the same review would have cost by pasting the whole standards
// document instead of retrieving from it.

import { Coins, Layers, Search } from "lucide-react";

import { StatTile } from "@/components/ui/stat-tile";

import {
  ANTHROPIC_REFERENCE_INPUT_TOKENS,
  ANTHROPIC_REFERENCE_IS_POPULATED,
  ANTHROPIC_REFERENCE_MODEL_ID,
  ANTHROPIC_REFERENCE_MODEL_LABEL,
  ANTHROPIC_REFERENCE_OUTPUT_TOKENS,
} from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { ORG_CODING_STANDARDS_CONTENT } from "../../data/adlcReviewAgentLab/orgCodingStandards";
import { computeSavingsPct, estimateCost } from "../../lib/bedrockPricing";
import { estimatePasteTokens } from "../../lib/retrievalCostEstimate";
import { useReviewRetrieval } from "../../hooks/useReviewRetrieval";
import { ContextStatsCard } from "../demo-apps/ContextStatsCard";
import { DatapackActivitySummary } from "../demo-apps/DatapackActivitySummary";
import { ReviewRetrievalSources } from "../demo-apps/ReviewRetrievalSources";
import { CostPayoffCard } from "../demo-apps/CostPayoffCard";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";
import { TokenCostBars } from "../demo-apps/TokenCostBars";
import { LabStageHeader } from "./LabStageHeader";

export function ReviewCostStage({
  run,
  datapackId,
  onGoToRunReview,
}: {
  run: PrReviewRunState | null;
  /** For the datapack-wide rollup below this run's own numbers. */
  datapackId: string | null;
  onGoToRunReview: () => void;
}) {
  // ContextStatsCard below reports HOW MANY chunks and memories reached the
  // model; this is the same turn's decision trace, which is the only place
  // their content lives. They belong together: a count with no way to see
  // what it counted is the question this page kept raising.
  const retrieval = useReviewRetrieval({
    datapackId,
    conversationId: run?.conversationId ?? null,
  });
  const usage = run?.reviewUsage ?? null;
  const stats = run?.contextStats ?? null;
  const modelId = run?.selectedModelId ?? null;

  if (!usage || !modelId) {
    return (
      <div className="flex flex-1 flex-col items-start gap-4 overflow-y-auto p-6">
        <LabStageHeader
          eyebrow="Chapter 3 · Step 3"
          title="What the context cost"
          lede="Run the review first. Every number on this page comes from that run."
        />
        <button type="button" onClick={onGoToRunReview} className="text-sm text-primary underline">
          Go to Run the review
        </button>
      </div>
    );
  }

  const modelLabel = MODEL_LABELS[modelId];
  const slmCost = estimateCost(modelId, usage);
  const claudeCost = ANTHROPIC_REFERENCE_IS_POPULATED
    ? estimateCost(ANTHROPIC_REFERENCE_MODEL_ID, {
        input_tokens: ANTHROPIC_REFERENCE_INPUT_TOKENS,
        output_tokens: ANTHROPIC_REFERENCE_OUTPUT_TOKENS,
      })
    : null;
  const savingsPct = computeSavingsPct(slmCost, claudeCost);

  // What this same review would have cost with the standards doc pasted in
  // whole: the measured non-retrieval input, plus the whole document.
  const retrievedTokens = (stats?.kb_tokens_estimated ?? 0) + (stats?.memory_tokens_estimated ?? 0);
  const promptWithoutContext = Math.max(usage.input_tokens - retrievedTokens, 0);
  const pasteTokens = promptWithoutContext + estimatePasteTokens(ORG_CODING_STANDARDS_CONTENT);
  const saved = pasteTokens - usage.input_tokens;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 3 · Step 3"
        title="What the context actually cost"
        lede={`Real token counts from the run you just did with ${modelLabel}.`}
        tags={["Metrics"]}
      />

      <div className="grid gap-3 sm:grid-cols-3">
        <StatTile
          icon={<Layers className="h-3 w-3" />}
          label="Input tokens"
          value={usage.input_tokens.toLocaleString()}
        />
        <StatTile
          icon={<Search className="h-3 w-3" />}
          label="Output tokens"
          value={usage.output_tokens.toLocaleString()}
        />
        <StatTile
          icon={<Coins className="h-3 w-3" />}
          label="Cost"
          value={slmCost === null ? "—" : `$${slmCost.toFixed(4)}`}
        />
      </div>

      {/* Only worth drawing when retrieval actually happened -- with both
          search tools disabled there is no second bar to compare against. */}
      {retrievedTokens > 0 && saved > 0 && (
        <section className="flex flex-col gap-3">
          <h3 className="text-sm font-semibold text-foreground">Input tokens per review</h3>
          <TokenCostBars
            bars={[
              {
                label: "Paste the whole standards doc into every prompt",
                tokens: pasteTokens,
                isBaseline: true,
                note: "estimated",
              },
              { label: "Query the datapack for what this diff needs", tokens: usage.input_tokens },
            ]}
          />
          <p className="text-xs text-muted-foreground">
            {saved.toLocaleString()} fewer input tokens on this review. The measured figure is real; the
            paste figure is estimated from the sample document's size, since nothing pasted it.
          </p>
        </section>
      )}

      {stats && (
        <ContextStatsCard
          stats={{
            kbChunksRetrieved: stats.kb_chunks_retrieved,
            kbChunksTotal: stats.kb_chunks_total,
            kbTokensEstimated: stats.kb_tokens_estimated,
            memoriesRetrieved: stats.memories_retrieved,
            memoriesTotal: stats.memories_total,
            memoryTokensEstimated: stats.memory_tokens_estimated,
          }}
        />
      )}

      <section className="flex flex-col gap-3">
        <h3 className="text-sm font-semibold text-foreground">What reached the model</h3>
        {retrieval.loading ? (
          <p className="text-xs text-muted-foreground">Loading the chunks and memories it read…</p>
        ) : retrieval.error ? (
          <p className="text-xs text-muted-foreground">{retrieval.error}</p>
        ) : (
          <ReviewRetrievalSources
            kbHits={retrieval.trace?.kb_hits ?? []}
            memoryHits={retrieval.trace?.memory_hits ?? []}
          />
        )}
      </section>

      {slmCost !== null && claudeCost !== null && savingsPct !== null && (
        <CostPayoffCard
          savingsPct={savingsPct}
          slmCost={slmCost}
          slmModelLabel={modelLabel}
          claudeCost={claudeCost}
          claudeModelLabel={ANTHROPIC_REFERENCE_MODEL_LABEL}
        />
      )}

      {/* Everything above is this one run. This is the datapack underneath it,
          which is what actually compounds: the numbers here outlive the
          session that produced them. */}
      <section className="flex flex-col gap-3 border-t border-border pt-6">
        <div>
          <h3 className="text-sm font-semibold text-foreground">And what the datapack has built up</h3>
          <p className="mt-1 text-xs text-muted-foreground">
            Read back from this datapack's own conversations and token accounting, not from this run.
          </p>
        </div>
        <DatapackActivitySummary datapackId={datapackId} />
      </section>
    </div>
  );
}
