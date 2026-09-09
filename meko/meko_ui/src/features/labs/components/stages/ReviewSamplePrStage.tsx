// Pure explainer -- shows exactly what AgentK is about to review before the
// user moves on to Claude's reference review and then runs their own. No
// data dependency (the diff is bundled sample content), so unlike
// run-review it isn't gated behind LabStageNeedsDatapack/LabStageNeedsDeployment.

import { FileDiff } from "lucide-react";

import {
  SAMPLE_DIFF_CONTENT,
  SAMPLE_DIFF_DESCRIPTION,
  SAMPLE_DIFF_TITLE,
} from "../../data/adlcReviewAgentLab/sampleDiff";
import { PrDiffViewer } from "../demo-apps/PrDiffViewer";

export function ReviewSamplePrStage({ agentDisplayName }: { agentDisplayName: string }) {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <div className="flex items-center gap-2">
        <FileDiff className="h-5 w-5 text-primary" aria-hidden />
        <h2 className="text-lg font-semibold text-foreground">Review the sample PR</h2>
      </div>
      <p className="text-sm text-muted-foreground">{agentDisplayName} reviews a bundled sample PR below</p>
      <PrDiffViewer
        title={SAMPLE_DIFF_TITLE}
        description={SAMPLE_DIFF_DESCRIPTION}
        diff={SAMPLE_DIFF_CONTENT}
        isSample
      />
    </div>
  );
}
