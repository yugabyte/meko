// Chapter 3's read of what the review actually found, alongside the Claude
// reference review for the same PR. Absorbs the old standalone
// ClaudeReferenceStage, which showed the reference with nothing to compare
// it against.

import { FileSearch, Quote } from "lucide-react";

import {
  ANTHROPIC_REFERENCE_FINDINGS,
  ANTHROPIC_REFERENCE_IS_POPULATED,
  ANTHROPIC_REFERENCE_MODEL_LABEL,
  type ReviewFinding,
} from "../../data/adlcReviewAgentLab/anthropicReferenceReview";
import { useReviewRetrieval } from "../../hooks/useReviewRetrieval";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { FindingsList } from "../demo-apps/FindingsList";
import { ReviewRetrievalSources } from "../demo-apps/ReviewRetrievalSources";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";
import { LabStageHeader } from "./LabStageHeader";

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

  if (!run || findings === null) {
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

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 3 · Step 2"
        title={
          findings.length > 0
            ? `${findings.length} finding${findings.length === 1 ? "" : "s"}, each citing your rules`
            : "No blocking issues found"
        }
        lede="Each comment traces back to a rule you loaded. If you disagree with one, you fix the rule, not the prompt."
        tags={modelLabel ? [modelLabel] : undefined}
      />

      <section className="flex flex-col gap-2">
        <h3 className="flex items-center gap-2 text-sm font-semibold text-foreground">
          <FileSearch className="h-4 w-4 text-primary" aria-hidden />
          {modelLabel ? `${modelLabel} · your run` : "Your run"}
        </h3>
        <FindingsList findings={findings as ReviewFinding[]} />
      </section>

      {/* The rules behind the comments. The trace records what the review
          retrieved for this PR, not which chunk produced which comment, so
          these sit under the whole set rather than against a line. */}
      <section className="flex flex-col gap-2">
        <h3 className="flex items-center gap-2 text-sm font-semibold text-foreground">
          <Quote className="h-4 w-4 text-primary" aria-hidden />
          What it checked against
        </h3>
        <p className="text-xs text-muted-foreground">
          Retrieved from your datapack for this PR, before it wrote a single comment.
        </p>
        {retrieval.loading ? (
          <p className="text-xs text-muted-foreground">Loading what this review retrieved…</p>
        ) : retrieval.error ? (
          <p className="text-xs text-muted-foreground">{retrieval.error}</p>
        ) : (
          <ReviewRetrievalSources
            variant="citations"
            kbHits={retrieval.trace?.kb_hits ?? []}
            memoryHits={retrieval.trace?.memory_hits ?? []}
          />
        )}
      </section>

      {/* The reference is a one-time capture, so it's a benchmark to read
          against rather than a second live result. Hidden entirely when it
          was never captured -- an empty list here would read as "Claude
          found nothing", which nobody measured. */}
      {ANTHROPIC_REFERENCE_IS_POPULATED && (
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-semibold text-foreground">
            {ANTHROPIC_REFERENCE_MODEL_LABEL} · reference
            <span className="ml-2 font-normal text-muted-foreground">· captured once, not a live run</span>
          </h3>
          <FindingsList findings={ANTHROPIC_REFERENCE_FINDINGS} />
        </section>
      )}

    </div>
  );
}
