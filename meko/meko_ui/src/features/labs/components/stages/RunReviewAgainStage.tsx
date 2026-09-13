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

import { PrReviewDemoApp, type PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageNeedsDeployment } from "../lab-runner/LabStageNeedsDeployment";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";

export function RunReviewAgainStage({
  demoApp,
  session,
  onGoToCreateDatapack,
  onGoToDeployAgent,
  savedRun,
  onRunChange,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
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
  return <PrReviewDemoApp demoApp={demoApp} session={session} savedRun={savedRun} onRunChange={onRunChange} />;
}
