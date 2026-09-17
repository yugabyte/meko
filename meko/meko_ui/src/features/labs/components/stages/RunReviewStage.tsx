import type { ReviewAgentConfig } from "../demo-apps/ReviewAgentConfigPanel";
import { PrReviewDemoApp, type PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageNeedsDeployment } from "../lab-runner/LabStageNeedsDeployment";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { LabStageHeader } from "./LabStageHeader";

export function RunReviewStage({
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
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="shrink-0 px-6 pt-6">
        <LabStageHeader
          eyebrow="Chapter 3 · Step 1"
          title="Same PR, same agent, new context"
          lede={`Nothing about the diff changed. The only difference is that ${demoApp.agentDisplayName} can now read your datapack.`}
          tags={["Live"]}
        />
      </div>
      <PrReviewDemoApp demoApp={demoApp} session={session} reviewConfig={reviewConfig} savedRun={savedRun} onRunChange={onRunChange} />
    </div>
  );
}
