// Chapter 2's opener: who the reviewer is, and the loop it sits in. The lab
// always runs the predeployed instance, so there is no deploy choice here --
// see the parked DeployAgentPanel below.

import { RefreshCw } from "lucide-react";

import { AgentIdentityCard } from "../demo-apps/AgentIdentityCard";
// PARKED with its usage below: import { DeployAgentPanel } from "../demo-apps/DeployAgentPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import { LabNote } from "./LabNote";
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
        title={`Meet ${demoApp.agentDisplayName}, the code review agent`}
        lede={`The reviewer. It runs on AWS Bedrock AgentCore, reads context over MCP, and knows nothing about your team until you hand it a datapack. It's open source, and this lab runs the same build you'd deploy yourself.`}
      />
      <AgentIdentityCard
        agentDisplayName={demoApp.agentDisplayName}
        agentSlug={demoApp.deployable.agentSlug}
        sourceRepoUrl={demoApp.deployable.sourceRepoUrl}
        deployment={session.deployment ?? null}
        hasContext={session.datapackGrant !== null}
      />

      <LabNote icon={RefreshCw}>
        <b className="font-semibold text-foreground">This is the ADLC loop.</b> The agent does the
        work; you set where it needs you. Chapter 2 sets both: the review goes straight to the
        agent, and you are asked for approval.
      </LabNote>

      {/* PARKED: the deploy-your-own picker (DeployAgentPanel, which wraps
          DeploymentModeChooser, RuntimeTargetPicker and the BYOC role
          wizard). The lab always runs the predeployed instance, which
          useLabRunnerSession already defaults `deploymentMode` to, so the
          picker was a choice with one real option and a paid-tier upsell
          beside it. Restoring it is uncommenting this and its import.
      <DeployAgentPanel
        agentSlug={demoApp.deployable.agentSlug}
        agentDisplayName={demoApp.agentDisplayName}
        sourceRepoUrl={demoApp.deployable.sourceRepoUrl}
        session={session}
      />
      */}
    </div>
  );
}
