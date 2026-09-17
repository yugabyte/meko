// Chapter 3's payoff, in two tabs. Cost is this one run: what it spent, and
// what the same review would have cost by pasting the whole standards
// document instead of retrieving from it. Metrics is the datapack underneath
// it, which is the half that outlives the session.

import { Coins, Layers, Search } from "lucide-react";

import { StatTile } from "@/components/ui/stat-tile";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";

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
import { ColdVsContextCards } from "../demo-apps/ColdVsContextCards";
import { ContextStatsCard } from "../demo-apps/ContextStatsCard";
import { DatapackActivitySummary } from "../demo-apps/DatapackActivitySummary";
import { CostPayoffCard } from "../demo-apps/CostPayoffCard";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";
import { TokenCostBars } from "../demo-apps/TokenCostBars";
import { LabStageHeader } from "./LabStageHeader";

export function ReviewCostStage({
  run,
  datapackId,
  datapackName,
  kbChunksTotal,
  kbChunkSize,
  onGoToRunReview,
}: {
  run: PrReviewRunState | null;
  /** For the datapack-wide rollup on the Metrics tab. */
  datapackId: string | null;
  /** Named on the with-context card, so the comparison says which datapack
   *  did the work rather than "with a datapack". */
  datapackName: string | null;
  /** Every chunk in the datapack, and the index's chunk size, both from the
   *  knowledge-base summary. `contextStats.kb_chunks_total` is the same
   *  datapack-wide total (it comes from datapack_describe, and is the
   *  denominator in "N of M chunks"), so it serves as the fallback -- the
   *  summary is preferred only because it is present whether or not a run
   *  reported stats. */
  kbChunksTotal: number | null;
  kbChunkSize: number | null;
  onGoToRunReview: () => void;
}) {
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

  // Structured findings are an array; "unstructured" means the agent replied
  // in prose, which is a different thing from finding nothing.
  const findings = Array.isArray(run?.reviewFindings) ? run.reviewFindings : null;
  const findingsCount = findings ? findings.length : null;
  const blockingCount = findings
    ? findings.filter((f) => f.severity === "critical" || f.severity === "major").length
    : null;

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
      />

      <ColdVsContextCards
        datapackName={datapackName ?? "your datapack"}
        findingsCount={findingsCount}
        blockingCount={blockingCount}
        inputTokens={usage.input_tokens}
        kbChunksTotal={kbChunksTotal ?? stats?.kb_chunks_total ?? null}
        kbChunkSize={kbChunkSize}
      />

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
                  {
                    label: "Query the datapack for what this diff needs",
                    tokens: usage.input_tokens,
                  },
                ]}
              />
              <p className="text-xs text-muted-foreground">
                {saved.toLocaleString()} fewer input tokens on this review. The measured figure is
                real; the paste figure is estimated from the sample document's size, since nothing
                pasted it.
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
          <div>
            <h3 className="text-sm font-semibold text-foreground">
              What the datapack has built up
            </h3>
            <p className="mt-1 text-xs text-muted-foreground">
              Read back from this datapack's own conversations and token accounting, not from this
              run.
            </p>
          </div>
          <DatapackActivitySummary datapackId={datapackId} />
        </TabsContent>
      </Tabs>
    </div>
  );
}
