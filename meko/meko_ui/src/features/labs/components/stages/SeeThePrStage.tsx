// Chapter 1's opener: the sample PR, reviewed by AgentK with no context at
// all. The baseline the rest of the lab is measured against.

import { useCallback, useState } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { WarningBanner } from "@/components/ui/warning-banner";

import {
  COLD_REVIEW_DURATION,
  COLD_REVIEW_FINDINGS,
  COLD_REVIEW_INPUT_TOKENS,
  COLD_REVIEW_IS_POPULATED,
  COLD_REVIEW_MISSED_SUMMARY,
  COLD_REVIEW_TRACE,
  COLD_REVIEW_VERDICT,
} from "../../data/adlcReviewAgentLab/coldReviewCapture";
import {
  SAMPLE_DIFF_CONTENT,
  SAMPLE_DIFF_DESCRIPTION,
  SAMPLE_DIFF_TITLE,
} from "../../data/adlcReviewAgentLab/sampleDiff";
import { ColdReviewTrace } from "../demo-apps/ColdReviewTrace";
import { PrDiffViewer } from "../demo-apps/PrDiffViewer";
import { LabStageHeader } from "./LabStageHeader";

export function SeeThePrStage({
  agentDisplayName,
  onComplete,
}: {
  agentDisplayName: string;
  /** Satisfies this stage's gate — the user has seen the baseline. */
  onComplete: () => void;
}) {
  const [started, setStarted] = useState(false);
  const [traceDone, setTraceDone] = useState(false);

  // Stable: ColdReviewTrace lists this in the deps of the effect that calls
  // it, so a fresh arrow per render would re-run that effect forever.
  const handleFinished = useCallback(() => setTraceDone(true), []);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 1"
        title="Here's the pull request"
        lede={`A teammate opened PR #418 on the billing service. Thirty-four lines, tests green, CI passing. Before you give ${agentDisplayName} anything, let it review the diff on its own.`}
        tags={["Sample PR"]}
      />

      <PrDiffViewer
        title={SAMPLE_DIFF_TITLE}
        description={SAMPLE_DIFF_DESCRIPTION}
        diff={SAMPLE_DIFF_CONTENT}
        isSample
      />

      <div className="flex flex-wrap items-center gap-3">
        <Button
          onClick={() => {
            setStarted(true);
            onComplete();
          }}
          disabled={started}
        >
          {started ? "Reviewed, 0 issues" : "Review with no context"}
        </Button>
        <span className="text-sm text-muted-foreground">
          {agentDisplayName}, cold, with no datapack attached
        </span>
      </div>

      {started && (
        <>
          <ColdReviewTrace lines={COLD_REVIEW_TRACE} onFinished={handleFinished} />

          {traceDone && (
            <>
              <div className="flex gap-3 rounded-md border border-border bg-card p-4">
                <Badge variant="secondary" className="h-fit shrink-0">
                  approved
                </Badge>
                <div className="min-w-0">
                  {COLD_REVIEW_IS_POPULATED ? (
                    <>
                      <p className="font-medium text-foreground">“{COLD_REVIEW_VERDICT}”</p>
                      <p className="mt-1 text-xs text-muted-foreground">
                        {agentDisplayName} · {COLD_REVIEW_DURATION} ·{" "}
                        {COLD_REVIEW_INPUT_TOKENS.toLocaleString()} input tokens ·{" "}
                        {COLD_REVIEW_FINDINGS.length} issues
                      </p>
                    </>
                  ) : (
                    // No capture recorded yet (see coldReviewCapture.ts). Say
                    // so rather than quoting a verdict nobody measured.
                    <p className="text-sm text-muted-foreground">
                      The baseline capture for this PR hasn't been recorded yet. The trace above shows
                      the shape of the run, not its measured result.
                    </p>
                  )}
                </div>
              </div>

              <WarningBanner tone="warning">{COLD_REVIEW_MISSED_SUMMARY}</WarningBanner>

            </>
          )}
        </>
      )}
    </div>
  );
}
