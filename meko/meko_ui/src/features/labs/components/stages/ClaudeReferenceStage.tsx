// Pure explainer -- shows Claude's already-captured reference review before
// the user configures and runs their own smaller model against the same
// context, so the comparison in run-review has a benchmark already in mind
// rather than appearing there for the first time. No data dependency (the
// reference is a static, one-time-captured fixture), so it isn't gated
// behind LabStageNeedsDatapack/LabStageNeedsDeployment.

import { Sparkles } from "lucide-react";

import { AnthropicReferenceCard } from "../demo-apps/AnthropicReferenceCard";

export function ClaudeReferenceStage({ agentDisplayName }: { agentDisplayName: string }) {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <div className="flex items-center gap-2">
        <Sparkles className="h-5 w-5 text-primary" aria-hidden />
        <h2 className="text-lg font-semibold text-foreground">See Claude's reference review</h2>
      </div>
      <p className="text-sm text-muted-foreground">
        Before you pick a smaller model for {agentDisplayName}, see what Claude Sonnet 4.5 already
        found reviewing this exact PR, grounded in the same coding standards you built in Chapter
        1 — this is the benchmark your model will be compared to.
      </p>
      <AnthropicReferenceCard />
    </div>
  );
}
