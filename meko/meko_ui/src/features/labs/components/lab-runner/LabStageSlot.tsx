import { useEffect, useRef, useState } from "react";

import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { BringYourOwnPrStage } from "../stages/BringYourOwnPrStage";
import { ClaudeReferenceStage } from "../stages/ClaudeReferenceStage";
import { CollaborateStage } from "../stages/CollaborateStage";
import { ContextIntroStage } from "../stages/ContextIntroStage";
import { ConversationStage } from "../stages/ConversationStage";
import { CreateAmpClusterStage } from "../stages/CreateAmpClusterStage";
import { CreateDatapackStage } from "../stages/CreateDatapackStage";
import { DashboardIntroStage } from "../stages/DashboardIntroStage";
import { DeployAgentStage } from "../stages/DeployAgentStage";
import { KnowledgeBaseStage } from "../stages/KnowledgeBaseStage";
import { MemoryStage } from "../stages/MemoryStage";
import { ReviewDashboardStage } from "../stages/ReviewDashboardStage";
import { ReviewSamplePrStage } from "../stages/ReviewSamplePrStage";
import { RunReviewAgainStage } from "../stages/RunReviewAgainStage";
import { RunReviewStage } from "../stages/RunReviewStage";
import { ShareWithAgentStage } from "../stages/ShareWithAgentStage";
import type { useLabProgress } from "../../hooks/useLabProgress";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDefinition } from "../../types";

/**
 * Main content pane of the lab runner: switches on progress.currentStage.id
 * (closed union, same convention the old LabDemoAppSlot used for
 * demoApp.kind) to render one of 18 fixed stages -- 16 distinct components,
 * since CreateDatapackStage (create-datapack, choose-datapack-again) and
 * ShareWithAgentStage (share-with-agent, share-with-agent-again) each cover
 * two stage ids.
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
  const goToCreateAmpCluster = () => progress.goToStageById("create-amp-cluster");

  // Owned here, not inside PrReviewDemoApp, because THIS component stays
  // mounted for the whole lab run while the switch below remounts whichever
  // stage component is current -- see PrReviewRunState's own doc comment.
  const [reviewRun, setReviewRun] = useState<PrReviewRunState | null>(null);
  // Chapter 3's own run, independent of Chapter 2's reviewRun above -- see
  // RunReviewAgainStage's own comment for why this is a separate state slot.
  const [dashboardReviewRun, setDashboardReviewRun] = useState<PrReviewRunState | null>(null);
  // Chapter 4's own run (a real, user-fetched PR) -- same reasoning, its own
  // independent slot rather than reusing either run above.
  const [byocReviewRun, setByocReviewRun] = useState<PrReviewRunState | null>(null);
  // Chapter 4's own choose-datapack-again stage can change session.datapackId
  // mid-chapter (unlike Chapter 2/3, whose only datapack choice is the
  // lab's very first stage) -- without this, a review already run against
  // the OLD datapack stays in byocReviewRun, so BringYourOwnPrStage keeps
  // showing it (fetch form unreachable) after switching to a datapack that
  // was never reviewed, and a follow-up question would attach the NEW
  // datapack_id to the OLD conversation_id. Guarded on an actual change (not
  // the initial mount) the same way useLabRunnerSession's own datapackId-
  // change eviction effect is.
  const prevDatapackIdRef = useRef(session.datapackId);
  useEffect(() => {
    if (prevDatapackIdRef.current === session.datapackId) return;
    prevDatapackIdRef.current = session.datapackId;
    setByocReviewRun(null);
  }, [session.datapackId]);

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
    case "dashboard-intro":
      return <DashboardIntroStage />;
    case "create-amp-cluster":
      return <CreateAmpClusterStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "run-review-again":
      return (
        <RunReviewAgainStage
          demoApp={lab.demoApp}
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToDeployAgent}
          savedRun={dashboardReviewRun}
          onRunChange={setDashboardReviewRun}
        />
      );
    case "review-dashboard":
      return (
        <ReviewDashboardStage
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToCreateAmpCluster={goToCreateAmpCluster}
        />
      );
    case "choose-datapack-again":
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
          heading="Choose a datapack"
          description="Keep reviewing against the same datapack, or switch to a different one you have access to -- this is what AgentK reviews your PR against."
        />
      );
    case "share-with-agent-again":
      return <ShareWithAgentStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "bring-your-own-pr":
      return (
        <BringYourOwnPrStage
          demoApp={lab.demoApp}
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToDeployAgent}
          savedRun={byocReviewRun}
          onRunChange={setByocReviewRun}
        />
      );
    default:
      return null;
  }
}
