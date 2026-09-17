// The lab's argument in two cards: the same diff reviewed with no datapack
// and with the one the user built. Shown at the top of the cost step, before
// any token arithmetic, because "it found the bug" is the claim the numbers
// then explain.

import { cn } from "@/lib/utils";

import {
  COLD_REVIEW_FINDINGS,
  COLD_REVIEW_INPUT_TOKENS,
  COLD_REVIEW_IS_POPULATED,
  COLD_REVIEW_VERDICT,
} from "../../data/adlcReviewAgentLab/coldReviewCapture";

export function ColdVsContextCards({
  datapackName,
  findingsCount,
  blockingCount,
  inputTokens,
}: {
  datapackName: string;
  /** Null when the run returned prose the agent didn't structure, so the
   *  count is unknown rather than zero. */
  findingsCount: number | null;
  blockingCount: number | null;
  inputTokens: number;
}) {
  return (
    <div className="grid gap-3 sm:grid-cols-2">
      <ComparisonCard
        eyebrow="No datapack"
        value={COLD_REVIEW_FINDINGS.length}
        valueTone="muted"
        caption={`issues found · “${COLD_REVIEW_VERDICT}”`}
        rows={[
          // The cold run's verdict is authored, its token count is not: until
          // someone records the capture there is no number to quote here.
          { label: "Input tokens", value: COLD_REVIEW_IS_POPULATED ? COLD_REVIEW_INPUT_TOKENS.toLocaleString() : "not measured" },
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
      <p className="font-mono text-[10px] uppercase tracking-wider text-muted-foreground">
        {eyebrow}
      </p>
      <div>
        <p
          className={cn(
            "text-3xl font-semibold tabular-nums",
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
                "font-mono text-xs tabular-nums",
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
