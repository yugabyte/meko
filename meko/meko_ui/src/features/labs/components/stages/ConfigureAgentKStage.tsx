// Chapter 2's last setup step: choose the model and the sources the review
// will run with. Split out of the run step so Chapter 3 is one button and a
// result, and so the choice reads as part of setting the agent up rather
// than as a form standing between the user and the review.

import { ReviewAgentConfigPanel, type ReviewAgentConfig } from "../demo-apps/ReviewAgentConfigPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageHeader } from "./LabStageHeader";

export function ConfigureAgentKStage({
  agentDisplayName,
  datapackId,
  config,
  onConfigChange,
  onGoToCreateDatapack,
}: {
  agentDisplayName: string;
  datapackId: string | null;
  /** Owned by LabStageSlot so Chapter 3's run reads the same object this
   *  step wrote — the stage components themselves remount on navigation. */
  config: ReviewAgentConfig;
  onConfigChange: (next: ReviewAgentConfig) => void;
  onGoToCreateDatapack: () => void;
}) {
  if (!datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 3"
        title={`Configure ${agentDisplayName}`}
        lede={`Every source in this datapack is live. Whichever model you pick reaches it the same way Claude did: by calling knowledgebase_search and memory_search against your datapack.`}
        tags={["Setup"]}
      />

      <ReviewAgentConfigPanel
        value={config}
        onChange={onConfigChange}
        agentDisplayName={agentDisplayName}
      />

      <p className="text-xs text-muted-foreground">
        These settings carry into the review in Chapter 3.
      </p>
    </div>
  );
}
