// Chapter 3's read of what the review found: each comment against the code it
// points at, with the rules behind it attached.
//
// The pairing of a rule to a comment is computed here from word overlap
// (lib/findingGrounding.ts), because the decision trace records what the turn
// retrieved and never which chunk produced which comment. The page says so
// under the list rather than letting the layout imply a citation the backend
// did not make.

import { useMemo } from "react";

import {
  ANTHROPIC_REFERENCE_FINDINGS,
  ANTHROPIC_REFERENCE_IS_POPULATED,
  ANTHROPIC_REFERENCE_MODEL_LABEL,
  type ReviewFinding,
} from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { useReviewRetrieval } from "../../hooks/useReviewRetrieval";
import { parseUnifiedDiff } from "../../lib/diffExcerpt";
import type { RetrievedSource } from "../../lib/findingGrounding";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { FindingsList } from "../demo-apps/FindingsList";
import { GroundedFindingCard } from "../demo-apps/GroundedFindingCard";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";
import { LabStageHeader } from "./LabStageHeader";

/** Severity order, so the blocking comments are read first. The agent
 *  returns them in the order it wrote them, which is file order. */
const SEVERITY_RANK: Record<ReviewFinding["severity"], number> = {
  critical: 0,
  major: 1,
  minor: 2,
};

export function ReviewFindingsStage({
  run,
  datapackId,
  onGoToRunReview,
}: {
  /** The Chapter 3 run, owned by LabStageSlot so it survives this stage
   *  unmounting — null until the user has actually run a review. */
  run: PrReviewRunState | null;
  datapackId: string | null;
  onGoToRunReview: () => void;
}) {
  const retrieval = useReviewRetrieval({
    datapackId,
    conversationId: run?.conversationId ?? null,
  });
  const findings = Array.isArray(run?.reviewFindings) ? run.reviewFindings : null;
  const modelLabel = run?.selectedModelId ? MODEL_LABELS[run.selectedModelId] : null;
  const diffContent = run?.diffContent ?? null;

  const diffFiles = useMemo(
    () => (diffContent ? parseUnifiedDiff(diffContent) : []),
    [diffContent],
  );

  const sources: RetrievedSource[] = useMemo(() => {
    const trace = retrieval.trace;
    if (!trace) return [];
    return [
      ...(trace.kb_hits ?? []).map((hit) => ({
        kind: "knowledge" as const,
        source: hit.document_name?.trim() || hit.document_id || "Unknown document",
        quote: hit.chunk_text,
      })),
      ...(trace.memory_hits ?? []).map((hit) => ({
        kind: "memory" as const,
        source: hit.kind === "promoted" ? "collective memory" : "your memory",
        quote: hit.text,
      })),
    ];
  }, [retrieval.trace]);

  const sorted = useMemo(
    () =>
      findings
        ? [...findings].sort((a, b) => SEVERITY_RANK[a.severity] - SEVERITY_RANK[b.severity])
        : null,
    [findings],
  );

  if (!run || sorted === null) {
    return (
      <div className="flex flex-1 flex-col items-start gap-4 overflow-y-auto p-6">
        <LabStageHeader
          eyebrow="Chapter 3 · Step 2"
          title="The findings"
          lede="Run the review first. This reads the findings it returns."
        />
        <button type="button" onClick={onGoToRunReview} className="text-sm text-primary underline">
          Go to Run the review
        </button>
      </div>
    );
  }

  const blocking = sorted.filter((f) => f.severity === "critical" || f.severity === "major").length;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 3 · Step 2"
        title={
          sorted.length > 0
            ? `${sorted.length} finding${sorted.length === 1 ? "" : "s"}, ${blocking} worth blocking on`
            : "No blocking issues found"
        }
        lede="Each comment sits against the lines it flagged, and names what the agent read before writing it. If you disagree with one, you fix the rule, not the prompt."
        tags={modelLabel ? [modelLabel] : undefined}
      />

      <div className="flex flex-col gap-4">
        {sorted.map((finding, i) => (
          <GroundedFindingCard
            key={i}
            finding={finding}
            diffFiles={diffFiles}
            sources={sources}
          />
        ))}
      </div>

      {retrieval.loading ? (
        <p className="text-xs text-muted-foreground">Loading what this review retrieved…</p>
      ) : retrieval.error ? (
        <p className="text-xs text-muted-foreground">{retrieval.error}</p>
      ) : sources.length === 0 ? (
        <p className="rounded-md border border-dashed border-border px-3 py-2.5 text-xs text-muted-foreground">
          This review matched nothing in your datapack, so every comment above is the model on its
          own.
        </p>
      ) : (
        <p className="text-xs text-muted-foreground">
          The trace records what this review retrieved, not which chunk produced which comment, so
          each rule above is paired to the comment it most closely matches.
        </p>
      )}

      {/* The reference is a one-time capture, so it's a benchmark to read
          against rather than a second live result. Hidden entirely when it
          was never captured -- an empty list here would read as "Claude
          found nothing", which nobody measured. */}
      {ANTHROPIC_REFERENCE_IS_POPULATED && (
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold text-foreground">
            {ANTHROPIC_REFERENCE_MODEL_LABEL} · reference
            <span className="ml-2 font-normal text-muted-foreground">
              · captured once, not a live run
            </span>
          </h3>
          <FindingsList findings={ANTHROPIC_REFERENCE_FINDINGS} />
        </section>
      )}
    </div>
  );
}
