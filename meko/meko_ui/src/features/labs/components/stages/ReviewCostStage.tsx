// Chapter 3's payoff, in two tabs. Cost is this one run: what the two ways of
// feeding a reviewer cost, side by side. Metrics is the datapack underneath
// it, shown through the product's own Analytics section, which is the half
// that outlives the session.

import { Coins, Layers, Search } from "lucide-react";

import { StatTile } from "@/components/ui/stat-tile";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { deriveDatapackMetrics } from "@/lib/datapackMetrics";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";

import {
  ANTHROPIC_REFERENCE_INPUT_TOKENS,
  ANTHROPIC_REFERENCE_IS_POPULATED,
  ANTHROPIC_REFERENCE_MODEL_ID,
  ANTHROPIC_REFERENCE_MODEL_LABEL,
  ANTHROPIC_REFERENCE_OUTPUT_TOKENS,
} from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { computeSavingsPct, estimateCost } from "../../lib/bedrockPricing";
import { countableFindings } from "../../lib/countableFindings";
import {
  coldContextTokens,
  FRONTIER_MODEL_ID,
  LAB_TOKENS_PER_CHUNK,
  splitInputTokens,
} from "../../lib/reviewCostModel";
import { ColdVsContextCards } from "../demo-apps/ColdVsContextCards";
import { ContextStatsCard } from "../demo-apps/ContextStatsCard";
import { CostPayoffCard } from "../demo-apps/CostPayoffCard";
import { LabDatapackMetricsPanel } from "../demo-apps/LabDatapackMetricsPanel";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";
import { LabStageHeader } from "./LabStageHeader";

const FRONTIER_LABEL = "Claude Sonnet 4.5";

export function ReviewCostStage({
  run,
  datapackId,
  onGoToRunReview,
}: {
  run: PrReviewRunState | null;
  /** Drives both the quality figure on the comparison and the whole Metrics
   *  tab below it. */
  datapackId: string | null;
  onGoToRunReview: () => void;
}) {
  const { data: metrics } = useDatapackMetrics(datapackId);
  const usage = run?.reviewUsage ?? null;
  const stats = run?.contextStats ?? null;
  const modelId = run?.selectedModelId ?? null;

  if (!usage || !modelId) {
    return (
      <div className="flex flex-1 flex-col items-start gap-4 overflow-y-auto p-6">
        <LabStageHeader
          eyebrow="Chapter 3 · Step 4"
          title="What the context cost"
          lede="Run the review first. Every number on this page comes from that run."
        />
        <button type="button" onClick={onGoToRunReview} className="text-sm text-primary underline">
          Go to Run the review
        </button>
      </div>
    );
  }

  // null covers prose, an unfinished run AND an empty list -- see
  // countableFindings. An empty list reaching here as "0 issues found" in the
  // good tone is the same false-clean claim this chapter exists to avoid.
  const findings = countableFindings(run?.reviewFindings);
  const findingsCount = findings ? findings.length : null;

  const modelLabel = MODEL_LABELS[modelId];
  const slmCost = estimateCost(modelId, usage);
  const claudeCost = ANTHROPIC_REFERENCE_IS_POPULATED
    ? estimateCost(ANTHROPIC_REFERENCE_MODEL_ID, {
        input_tokens: ANTHROPIC_REFERENCE_INPUT_TOKENS,
        output_tokens: ANTHROPIC_REFERENCE_OUTPUT_TOKENS,
      })
    : null;
  const savingsPct = computeSavingsPct(slmCost, claudeCost);

  // The with-datapack side is what the run actually sent. The no-datapack
  // side is an estimate of what it would have had to send: every chunk in the
  // knowledge base, because there is no way to retrieve a subset without one.
  const coldTokens = coldContextTokens(stats?.kb_chunks_total);
  const split = stats
    ? splitInputTokens(usage.input_tokens, stats.kb_chunks_retrieved, stats.memories_retrieved)
    : null;
  // The datapack's own retrieval quality, not this run's -- a single review
  // has too few scored hits to average.
  const derived = deriveDatapackMetrics(metrics, {
    knowledgeCount: 0,
    memoryCount: 0,
    collectiveMemoryCount: 0,
  });
  const qualityScore = derived.scores.total > 0 ? derived.qualityScore : null;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 3 · Step 4"
        title="What the context actually cost"
        lede={`Real token counts from the run you just did with ${modelLabel}.`}
      />

      <ColdVsContextCards
        findingsCount={findingsCount}
        contextTokens={usage.input_tokens}
        coldTokens={coldTokens}
        qualityScore={qualityScore}
        modelLabel={modelLabel}
        modelId={modelId}
        frontierModelLabel={FRONTIER_LABEL}
        frontierModelId={FRONTIER_MODEL_ID}
      />

      {/* One side is measured and the other is not, which the reader cannot
          tell from the numbers alone. */}
      <p className="text-xs text-muted-foreground">
        The with-datapack figure is what this run actually sent. The no-datapack figure is an
        estimate: every chunk in your knowledge base, priced at a round{" "}
        {LAB_TOKENS_PER_CHUNK.toLocaleString()} tokens each, because a review with nothing to query
        has to carry all of it.
      </p>

      <Tabs defaultValue="cost" className="flex flex-col gap-6">
        <TabsList className="w-auto justify-start">
          <TabsTrigger value="cost">Cost</TabsTrigger>
          <TabsTrigger value="metrics">Metrics</TabsTrigger>
        </TabsList>

        <TabsContent value="cost" className="mt-0 flex flex-col gap-6">
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

          {stats && (
            <ContextStatsCard
              stats={{
                kbChunksRetrieved: stats.kb_chunks_retrieved,
                kbChunksTotal: stats.kb_chunks_total,
                // This run's own input, divided between the two searches
                // rather than re-estimated, so the bars add up to the number
                // the Cost tab reports directly above them.
                kbTokensEstimated: split?.kb ?? stats.kb_tokens_estimated,
                memoriesRetrieved: stats.memories_retrieved,
                memoriesTotal: stats.memories_total,
                memoryTokensEstimated: split?.memory ?? stats.memory_tokens_estimated,
              }}
              note="Chunk and memory counts are exact. The token split apportions this run's input across the two searches, weighted toward knowledge; the prompt also carries the diff itself."
            />
          )}

          {slmCost !== null && claudeCost !== null && savingsPct !== null && (
            <CostPayoffCard
              savingsPct={savingsPct}
              slmCost={slmCost}
              slmModelLabel={modelLabel}
              claudeCost={claudeCost}
              claudeModelLabel={ANTHROPIC_REFERENCE_MODEL_LABEL}
            />
          )}
        </TabsContent>

        {/* The Cost tab is this one run. This is the datapack underneath it,
            which is what actually compounds: these numbers outlive the session
            that produced them. */}
        <TabsContent value="metrics" className="mt-0 flex flex-col gap-3">
          <LabDatapackMetricsPanel datapackId={datapackId} />
        </TabsContent>
      </Tabs>
    </div>
  );
}
