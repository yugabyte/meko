import { useState } from "react";

import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { ClaudeReferenceStage } from "../stages/ClaudeReferenceStage";
import { CollaborateStage } from "../stages/CollaborateStage";
import { ContextIntroStage } from "../stages/ContextIntroStage";
import { ConversationStage } from "../stages/ConversationStage";
import { CreateDatapackStage } from "../stages/CreateDatapackStage";
import { DeployAgentStage } from "../stages/DeployAgentStage";
import { KnowledgeBaseStage } from "../stages/KnowledgeBaseStage";
import { MemoryStage } from "../stages/MemoryStage";
import { ReviewSamplePrStage } from "../stages/ReviewSamplePrStage";
import { RunReviewStage } from "../stages/RunReviewStage";
import { ShareWithAgentStage } from "../stages/ShareWithAgentStage";
import type { useLabProgress } from "../../hooks/useLabProgress";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDefinition } from "../../types";

/**
 * Main content pane of the lab runner: switches on progress.currentStage.id
 * (closed union, same convention the old LabDemoAppSlot used for
 * demoApp.kind) to render one of the 11 fixed stage components.
 */
export function LabStageSlot({
  lab,
  session,
  progress,
}: {
  lab: LabDefinition;
  session: ReturnType<typeof useLabRunnerSession>;
  progress: ReturnType<typeof useLabProgress>;
}) {
  const goToCreateDatapack = () => progress.goToStageById("create-datapack");
  const goToDeployAgent = () => progress.goToStageById("deploy-agent");

  // Owned here, not inside PrReviewDemoApp, because THIS component stays
  // mounted for the whole lab run while the switch below remounts whichever
  // stage component is current -- see PrReviewRunState's own doc comment.
  const [reviewRun, setReviewRun] = useState<PrReviewRunState | null>(null);

  switch (progress.currentStage.id) {
    case "create-datapack":
      return (
        <CreateDatapackStage
          datapacks={session.accessibleDatapacks}
          loading={session.datapacksLoading}
          selectedId={session.datapackId}
          onSelect={session.selectDatapack}
          onCreate={session.createAndSelectDatapack}
          creating={session.creating}
          hasHitDatapackCount={session.hasHitDatapackCount}
          onContinue={progress.goToNext}
        />
      );
    case "context-intro":
      return <ContextIntroStage />;
    case "knowledge-base":
      return <KnowledgeBaseStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "memory":
      return <MemoryStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "conversation":
      return <ConversationStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "deploy-agent":
      return (
        <DeployAgentStage demoApp={lab.demoApp} session={session} onGoToCreateDatapack={goToCreateDatapack} />
      );
    case "share-with-agent":
      return <ShareWithAgentStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "review-sample-pr":
      return <ReviewSamplePrStage agentDisplayName={lab.demoApp.agentDisplayName} />;
    case "claude-reference":
      return <ClaudeReferenceStage agentDisplayName={lab.demoApp.agentDisplayName} />;
    case "run-review":
      return (
        <RunReviewStage
          demoApp={lab.demoApp}
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToDeployAgent}
          savedRun={reviewRun}
          onRunChange={setReviewRun}
        />
      );
    case "collaborate":
      return <CollaborateStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    default:
      return null;
  }
}
