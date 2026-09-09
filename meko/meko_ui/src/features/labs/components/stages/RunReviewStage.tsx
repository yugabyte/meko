import { PrReviewDemoApp, type PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageNeedsDeployment } from "../lab-runner/LabStageNeedsDeployment";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";

export function RunReviewStage({
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
  /** Threaded straight through to PrReviewDemoApp -- see its own doc
   *  comment on PrReviewRunState/savedRun for why LabStageSlot owns this. */
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
