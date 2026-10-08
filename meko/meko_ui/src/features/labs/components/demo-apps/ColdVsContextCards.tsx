// The lab's argument in two cards: the same diff reviewed with no datapack
// and with the one the user built. Shown at the top of the cost step, before
// any token arithmetic, because "it found the bug" is the claim the numbers
// then explain.
//
// Same rows in the same order on both sides, so the two can be read across
// rather than each on its own.

import { cn } from "@/lib/utils";

import { BEDROCK_PRICE_PER_MILLION_TOKENS } from "../../lib/bedrockPricing";

type Row = { label: string; value: string; tone?: "good" | "bad" };

/** "$3.00 / $15.00" -- input rate then output rate, per million tokens.
 *  A model with no published rate reads as unknown, never as free. */
function ratePerMillion(modelId: string | null): string {
  const rate = modelId ? BEDROCK_PRICE_PER_MILLION_TOKENS[modelId] : undefined;
  if (!rate) return "—";
  return `$${rate.input.toFixed(2)} / $${rate.output.toFixed(2)}`;
}

export function ColdVsContextCards({
  findingsCount,
  contextTokens,
  coldTokens,
  qualityScore,
  modelLabel,
  modelId,
  frontierModelLabel,
  frontierModelId,
}: {
  /** Null when no count can honestly be claimed -- prose, an unfinished run,
   *  or an empty list, which means either a clean diff or a response the
   *  agent could not read. See lib/countableFindings. */
  findingsCount: number | null;
  /** Context tokens the retrieval sent, and what the same review carries
   *  with no datapack to query. Null when nothing was retrieved. */
  contextTokens: number | null;
  coldTokens: number | null;
  /** The datapack's own retrieval quality, 0-1, from its metrics. Null
   *  before anything has been scored. */
  qualityScore: number | null;
  /** The small model the user picked in Chapter 2, which is the one that
   *  actually ran. */
  modelLabel: string;
  modelId: string | null;
  frontierModelLabel: string;
  frontierModelId: string;
}) {
  const coldRows: Row[] = [
    {
      label: "Input tokens (whole KB in the prompt)",
      value: coldTokens === null ? "—" : `${coldTokens.toLocaleString()} est.`,
    },
    { label: "Context quality", value: "Lower", tone: "bad" },
    { label: "Model used", value: frontierModelLabel },
    { label: "Model cost (in / out per M)", value: ratePerMillion(frontierModelId) },
    { label: "Learnings", value: "A new doc, redistributed every time" },
  ];

  const contextRows: Row[] = [
    {
      label: "Input tokens (composed via context search)",
      value: contextTokens === null ? "—" : contextTokens.toLocaleString(),
    },
    {
      label: "Context quality",
      value: qualityScore === null ? "High" : `High · ${Math.round(qualityScore * 100)}%`,
      tone: "good",
    },
    { label: "Model used", value: modelLabel },
    { label: "Model cost (in / out per M)", value: ratePerMillion(modelId) },
    { label: "Learnings", value: "The next agent inherits them" },
  ];

  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <ComparisonCard
        eyebrow="No Meko Datapack"
        headline="No issues found"
        headlineTone="bad"
        rows={coldRows}
      />
      <ComparisonCard
        eyebrow="With Meko Datapack"
        headline={
          findingsCount === null
            ? "No count to report"
            : `${findingsCount} issue${findingsCount === 1 ? "" : "s"} found`
        }
        headlineTone={findingsCount === null ? "muted" : "good"}
        rows={contextRows}
        highlighted
      />
    </div>
  );
}

function ComparisonCard({
  eyebrow,
  headline,
  headlineTone,
  rows,
  highlighted,
}: {
  eyebrow: string;
  headline: string;
  headlineTone: "good" | "bad" | "muted";
  rows: Row[];
  highlighted?: boolean;
}) {
  return (
    <section
      className={cn(
        "flex flex-col gap-3 rounded-lg border p-4",
        highlighted ? "border-context-knowledge/40 bg-card" : "border-border bg-card",
      )}
    >
      <p className="text-[10px] font-semibold uppercase tracking-wider text-muted-foreground">
        {eyebrow}
      </p>
      <p
        className={cn(
          "text-xl font-semibold",
          headlineTone === "good" && "text-context-knowledge",
          headlineTone === "bad" && "text-destructive",
          headlineTone === "muted" && "text-muted-foreground",
        )}
      >
        {headline}
      </p>
      <dl className="flex flex-col divide-y divide-border border-t border-border">
        {rows.map((row) => (
          <div key={row.label} className="flex items-center justify-between gap-3 py-2">
            <dt className="text-xs text-muted-foreground">{row.label}</dt>
            <dd
              className={cn(
                "shrink-0 text-right text-xs tabular-nums",
                row.tone === "good" && "text-context-knowledge",
                row.tone === "bad" && "text-destructive",
                !row.tone && "text-foreground",
              )}
            >
              {row.value}
            </dd>
          </div>
        ))}
      </dl>
    </section>
  );
}
