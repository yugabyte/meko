// Chapter 3's own "run a review" -- deliberately a SEPARATE stage from
// Chapter 2's run-review, with its own independent PrReviewRunState (owned
// by LabStageSlot, passed in as savedRun/onRunChange -- same
// navigate-away-and-back persistence Chapter 2's run-review stage already
// has, not the same state slot). Zero changes needed to PrReviewDemoApp.tsx
// itself -- same props it already accepts.
//
// This run isn't recorded anywhere yet: writing a real row into the AMP
// cluster created in the previous stage needs the still-unbuilt storage
// backend (see chapter3-plan-a-meko.md / chapter3-plan-b-amp-controller.md,
// uploaded to the meko-labs-chapter3 datapack) -- for now this stage exists
// so there's a second, real review run to look at, and review-dashboard
// shows illustrative sample data instead of this run's actual numbers.

import type { ReviewAgentConfig } from "../demo-apps/ReviewAgentConfigPanel";
import { PrReviewDemoApp, type PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageNeedsDeployment } from "../lab-runner/LabStageNeedsDeployment";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { LabStageHeader } from "./LabStageHeader";

export function RunReviewAgainStage({
  demoApp,
  session,
  reviewConfig,
  onGoToCreateDatapack,
  onGoToDeployAgent,
  savedRun,
  onRunChange,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  /** Chosen in Chapter 2's Configure step; threaded straight through. */
  reviewConfig: ReviewAgentConfig;
  onGoToCreateDatapack: () => void;
  onGoToDeployAgent: () => void;
  savedRun?: PrReviewRunState | null;
  onRunChange?: (run: PrReviewRunState) => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!session.deploymentActive) {
    return (
      <LabStageNeedsDeployment
        onGoToDeployAgent={onGoToDeployAgent}
        deploymentFailed={session.deployment?.status === "failed"}
      />
    );
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="shrink-0 px-6 pt-6">
        <LabStageHeader
          eyebrow="Chapter 4 · Step 2"
          title="Run another review"
          lede="One more review, so the dashboard's per-session table has more than a single row to show."
        />
      </div>
      <PrReviewDemoApp demoApp={demoApp} session={session} reviewConfig={reviewConfig} savedRun={savedRun} onRunChange={onRunChange} />
    </div>
  );
}
