// The lab's argument in two cards: the same diff reviewed with no datapack
// and with the one the user built. Shown at the top of the cost step, before
// any token arithmetic, because "it found the bug" is the claim the numbers
// then explain.

import { cn } from "@/lib/utils";

import { COLD_REVIEW_FINDINGS, COLD_REVIEW_VERDICT } from "../../data/adlcReviewAgentLab/coldReviewCapture";
import { estimatePasteTokensFromDocuments } from "../../lib/retrievalCostEstimate";

export function ColdVsContextCards({
  datapackName,
  findingsCount,
  blockingCount,
  inputTokens,
  kbChunksTotal,
  kbChunkSize,
}: {
  datapackName: string;
  /** Null when the run returned prose the agent didn't structure, so the
   *  count is unknown rather than zero. */
  findingsCount: number | null;
  blockingCount: number | null;
  inputTokens: number;
  /** Every chunk in the datapack, and the index's own chunk size. A review
   *  that cannot query the datapack has to be handed all of it, and that is
   *  what this prices -- through the same estimator Chapter 2 uses, so the
   *  two chapters cannot quote the same knowledge base differently. */
  kbChunksTotal: number | null;
  kbChunkSize: number | null;
}) {
  const coldTokens =
    estimatePasteTokensFromDocuments(
      [{ chunks_processed: kbChunksTotal ?? 0 }],
      kbChunkSize,
    ) ?? 0;
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <ComparisonCard
        eyebrow="No datapack"
        value={COLD_REVIEW_FINDINGS.length}
        valueTone="muted"
        caption={`issues found · “${COLD_REVIEW_VERDICT}”`}
        rows={[
          // NOT the cold run's own spend -- that capture is unmeasured, and
          // the run read nothing. This is what the same review costs without
          // a datapack to query: the whole knowledge base in the prompt. The
          // label says so, because the number is meaningless attributed to
          // the run above it.
          {
            label: "Whole KB in the prompt",
            value: coldTokens > 0 ? `${coldTokens.toLocaleString()} est.` : "not measured",
          },
          { label: "Vulnerability", value: "shipped", tone: "bad" },
        ]}
      />
      <ComparisonCard
        eyebrow={`With ${datapackName}`}
        value={findingsCount ?? "—"}
        valueTone="good"
        caption={
          findingsCount === null
            ? "the agent answered in prose, so nothing was counted"
            : blockingCount === null
              ? "issues found, each citing the rule it came from"
              : `issues found · ${blockingCount} blocking, each citing its rule`
        }
        rows={[
          { label: "Input tokens", value: inputTokens.toLocaleString() },
          { label: "Vulnerability", value: "caught pre-merge", tone: "good" },
        ]}
        highlighted
      />
    </div>
  );
}

function ComparisonCard({
  eyebrow,
  value,
  valueTone,
  caption,
  rows,
  highlighted,
}: {
  eyebrow: string;
  value: number | string;
  valueTone: "muted" | "good";
  caption: string;
  rows: { label: string; value: string; tone?: "good" | "bad" }[];
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
      <div>
        <p
          className={cn(
            "text-2xl font-semibold tabular-nums",
            valueTone === "good" ? "text-context-knowledge" : "text-muted-foreground",
          )}
        >
          {value}
        </p>
        <p className="mt-1 text-xs text-muted-foreground">{caption}</p>
      </div>
      <dl className="flex flex-col divide-y divide-border border-t border-border">
        {rows.map((row) => (
          <div key={row.label} className="flex items-center justify-between gap-3 py-2">
            <dt className="text-xs text-muted-foreground">{row.label}</dt>
            <dd
              className={cn(
                "text-xs tabular-nums",
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
