// Chapter 2's last setup step, and the lesson the cost argument rests on.
// Absorbs the old Configure-AgentK and How-it-reads-the-standards steps:
// they were one decision split across two screens, and the second could not
// show what the first had chosen.
//
// Two choices, in the order they matter. How the agent reads context moves
// the input side by more than an order of magnitude; the model then prices
// those tokens. Picking retrieval is what makes a small model enough, which
// is the whole claim — so the strategy comes first and the models read
// against it.

import { Check, Info } from "lucide-react";

import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { formatTokens } from "@/lib/tokenSavings";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";

import {
  contextTokens,
  costMultiple,
  FRONTIER_MODEL_ID,
  reviewCost,
} from "../../lib/reviewCostModel";
import {
  MODEL_OPTIONS,
  ReviewAgentConfigPanel,
  type ReviewAgentConfig,
} from "../demo-apps/ReviewAgentConfigPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabNote } from "./LabNote";
import { LabStageHeader } from "./LabStageHeader";

const FRONTIER_LABEL = "Claude Sonnet 4.5";

export function HowAgentReviewsStage({
  agentDisplayName,
  datapackId,
  config,
  onConfigChange,
  onGoToCreateDatapack,
}: {
  agentDisplayName: string;
  datapackId: string | null;
  /** Owned by LabStageSlot so Chapter 3's run reads the same object this
   *  step wrote — the stage components themselves remount on navigation. */
  config: ReviewAgentConfig;
  onConfigChange: (next: ReviewAgentConfig) => void;
  onGoToCreateDatapack: () => void;
}) {
  // The comparison below is (chunks in the knowledge base / chunks a review
  // retrieves), so it needs the real chunk count. Hooks run before the
  // datapack guard, which is why this sits above the early return.
  const { data: metrics } = useDatapackMetrics(datapackId);
  const kbChunks = metrics?.kb_chunk_count ?? null;

  if (!datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  // "Reads the datapack" is the pair of search tools being on. Chapter 3
  // sends exactly these, so the choice here is the run, not a picture of it.
  const useDatapack = config.useKnowledgeBase || config.useMemories;
  const setUseDatapack = (next: boolean) =>
    onConfigChange({ ...config, useKnowledgeBase: next, useMemories: next });

  const context = contextTokens(kbChunks);
  const chosen = reviewCost(config.model, useDatapack, kbChunks);
  const frontierNoPack = reviewCost(FRONTIER_MODEL_ID, false, kbChunks);
  const savings = costMultiple(frontierNoPack, chosen);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 3"
        title={`How ${agentDisplayName} reviews`}
        lede="Two choices decide what a review costs: what the agent reads, and which model reads it. The first one moves the numbers far more than the second."
        tags={["Setup"]}
      />

      <section className="flex flex-col gap-3">
        <h3 className="text-sm font-semibold text-foreground">How should it read the context?</h3>
        <div className="grid gap-3 sm:grid-cols-2">
          <StrategyCard
            title="Use datapack as context"
            recommended
            tooltip="The agent searches your datapack and pulls back only the rules that match this diff. One retrieval instead of the whole document, and every comment can name the rule behind it."
            body={`${agentDisplayName} searches and sends only what matches this diff: ${context.matched} of your ${context.kbChunks} chunks.`}
            cost={reviewCost(config.model, true, kbChunks)}
            selected={useDatapack}
            onSelect={() => setUseDatapack(true)}
          />
          {/* Priced on the FRONTIER model, not the one selected below. The
              two cards are a comparison of what a team actually does: with no
              context it can trust, it reaches for the biggest model, which is
              the choice this whole lesson is arguing against. Pricing both
              sides on the same small model understated the gap by hiding
              half of it. */}
          <StrategyCard
            title="Without datapack"
            body={`No search, so all ${context.kbChunks} chunks ride along in every prompt, on every retry, for every agent. Priced on ${FRONTIER_LABEL}, the model you reach for when you cannot trust the context.`}
            cost={reviewCost(FRONTIER_MODEL_ID, false, kbChunks)}
            selected={!useDatapack}
            onSelect={() => setUseDatapack(false)}
          />
        </div>
      </section>

      <LabNote icon={Info}>
        The datapack you built has high enough context efficiency that a small language model
        reaches the same answer as a frontier one. That is what retrieval buys: the model stops
        paying to re-read what it could have looked up.
      </LabNote>

      <section className="flex flex-col gap-3">
        <h3 className="text-sm font-semibold text-foreground">Which model runs the review?</h3>
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {MODEL_OPTIONS.map((option) => (
            <ModelCard
              key={option.value}
              label={option.label}
              tag="SLM"
              cost={reviewCost(option.value, useDatapack, kbChunks)}
              selected={config.model === option.value}
              onSelect={() => onConfigChange({ ...config, model: option.value })}
            />
          ))}
          {/* Shown, not selectable: it is the baseline the saving is measured
              against, and running it would change what Chapter 3 executes.
              Priced with NO datapack whatever the strategy above is set to,
              because that is the baseline -- the frontier model is what you
              reach for instead of context, not as well as it. Pricing it on
              the selected strategy put a different number here from the one
              the savings line below was dividing by. */}
          <ModelCard
            label={FRONTIER_LABEL}
            tag="Frontier"
            cost={frontierNoPack}
            selected={false}
            comparisonOnly
            comparisonNote="no datapack"
          />
        </div>

        {savings !== null && savings > 1 && (
          <p className="text-sm text-muted-foreground">
            Your selection costs{" "}
            <span className="font-semibold text-foreground">
              {savings >= 10 ? Math.round(savings) : savings.toFixed(1)}× less
            </span>{" "}
            per review than {FRONTIER_LABEL} with no datapack.
          </p>
        )}
      </section>

      <ReviewAgentConfigPanel
        value={config}
        onChange={onConfigChange}
        agentDisplayName={agentDisplayName}
        hideModel
        hideTools
      />
    </div>
  );
}

/** Quality / input tokens / cost, the three numbers both halves of this
 *  lesson move. Same three on every card so two cards can be read against
 *  each other without hunting for the matching row. */
function CostRows({ cost }: { cost: ReturnType<typeof reviewCost> }) {
  return (
    <dl className="flex flex-col gap-1 text-xs">
      <Row
        label="Quality"
        value={
          <span className={cost.quality === "high" ? "text-success" : "text-destructive"}>
            {cost.quality === "high" ? "High" : "Low"}
          </span>
        }
      />
      <Row label="Input tokens" value={formatTokens(cost.inputTokens)} />
      <Row
        label="Cost / review"
        value={cost.dollars === null ? "—" : `$${cost.dollars.toFixed(4)}`}
      />
    </dl>
  );
}

function Row({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="flex items-baseline justify-between gap-3">
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="font-mono tabular-nums text-foreground">{value}</dd>
    </div>
  );
}

function StrategyCard({
  title,
  body,
  cost,
  selected,
  onSelect,
  recommended,
  tooltip,
}: {
  title: string;
  body: string;
  cost: ReturnType<typeof reviewCost>;
  selected: boolean;
  onSelect: () => void;
  recommended?: boolean;
  tooltip?: string;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-pressed={selected}
      className={cn(
        "flex flex-col gap-3 rounded-lg border p-4 text-left transition-colors",
        selected
          ? "border-primary bg-primary/[0.04]"
          : "border-border bg-card hover:border-foreground/25",
      )}
    >
      <div className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-semibold text-foreground">{title}</span>
        {recommended && (
          <Badge variant="success" className="text-[10px]">
            recommended
          </Badge>
        )}
        {tooltip && (
          <TooltipProvider delayDuration={100}>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="cursor-help text-muted-foreground">
                  <Info className="h-3.5 w-3.5" aria-label="Why this is better" />
                </span>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[260px] leading-snug">
                {tooltip}
              </TooltipContent>
            </Tooltip>
          </TooltipProvider>
        )}
        {selected && <Check className="ml-auto h-4 w-4 shrink-0 text-primary" aria-hidden />}
      </div>
      {/* flex-1 so the body absorbs the height difference and the two cards'
          stat rows line up. The copy either side is a different number of
          lines, and rows that start at different heights cannot be read
          across, which is the only thing these cards are for. */}
      <p className="flex-1 text-xs leading-relaxed text-muted-foreground">{body}</p>
      <CostRows cost={cost} />
    </button>
  );
}

function ModelCard({
  label,
  tag,
  cost,
  selected,
  onSelect,
  comparisonOnly,
  comparisonNote,
}: {
  label: string;
  tag: string;
  cost: ReturnType<typeof reviewCost>;
  selected: boolean;
  onSelect?: () => void;
  comparisonOnly?: boolean;
  /** Why this card's figures differ from the selectable ones beside it. */
  comparisonNote?: string;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      disabled={comparisonOnly}
      aria-pressed={selected}
      className={cn(
        "flex flex-col gap-2 rounded-lg border p-3 text-left transition-colors",
        selected ? "border-primary bg-primary/[0.04]" : "border-border bg-card",
        comparisonOnly ? "opacity-70" : "hover:border-foreground/25",
      )}
    >
      <div className="flex items-center gap-2">
        <span
          className={cn(
            "flex h-4 w-4 shrink-0 items-center justify-center rounded-sm border",
            selected ? "border-primary bg-primary text-primary-foreground" : "border-border",
          )}
          aria-hidden
        >
          {selected && <Check className="h-3 w-3" />}
        </span>
        <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">{label}</span>
      </div>
      <Badge variant="secondary" className="w-fit text-[10px]">
        {comparisonOnly ? (comparisonNote ?? "comparison only") : tag}
      </Badge>
      <CostRows cost={cost} />
    </button>
  );
}
