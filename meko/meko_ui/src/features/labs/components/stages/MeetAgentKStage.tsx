// Chapter 2's opener: who the reviewer is and how to get one running —
// the shared instance, or your own. Replaces the old DeployAgentStage:
// Chapter 1's cold review now sets the premise, so this no longer leads
// with a with/without-context comparison (and no longer needs the
// scroll-collapse latch that kept that comparison out of the way).

import { AgentIdentityCard } from "../demo-apps/AgentIdentityCard";
import { DeployAgentPanel } from "../demo-apps/DeployAgentPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { LabStageHeader } from "./LabStageHeader";

export function MeetAgentKStage({
  demoApp,
  session,
  onGoToCreateDatapack,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!demoApp.deployable) {
    return <p className="p-6 text-sm text-muted-foreground">This lab has nothing to deploy.</p>;
  }

  return (
    <div className="flex min-h-0 flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 2 · Step 1"
        title={`Meet ${demoApp.agentDisplayName}`}
        lede={`The reviewer. It runs on AWS Bedrock AgentCore, reads context over MCP, and knows nothing about your team until you hand it a datapack. It's open source, and this lab runs the same build you'd deploy yourself.`}
      />
      <AgentIdentityCard
        agentDisplayName={demoApp.agentDisplayName}
        agentSlug={demoApp.deployable.agentSlug}
        sourceRepoUrl={demoApp.deployable.sourceRepoUrl}
        deployment={session.deployment ?? null}
        hasContext={session.datapackGrant !== null}
      />
      <DeployAgentPanel
        agentSlug={demoApp.deployable.agentSlug}
        agentDisplayName={demoApp.agentDisplayName}
        sourceRepoUrl={demoApp.deployable.sourceRepoUrl}
        session={session}
      />
    </div>
  );
}
