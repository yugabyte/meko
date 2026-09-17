// Main content pane of the lab runner: switches on progress.currentStage.id
// (closed union) to render one concrete stage component, and hands each
// gated stage the callback that satisfies its gate.

import { useCallback, useEffect, useRef, useState } from "react";

import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import {
  DEFAULT_REVIEW_AGENT_CONFIG,
  type ReviewAgentConfig,
} from "../demo-apps/ReviewAgentConfigPanel";
import { CollaborateStage } from "../stages/CollaborateStage";
import { ConfigureAgentKStage } from "../stages/ConfigureAgentKStage";
import { ConversationStage } from "../stages/ConversationStage";
// PARKED with the create-amp-cluster stage, see chapters.ts.
// import { CreateAmpClusterStage } from "../stages/CreateAmpClusterStage";
import { CreateDatapackStage } from "../stages/CreateDatapackStage";
// PARKED with chapter 4, see chapters.ts.
// import { DashboardIntroStage } from "../stages/DashboardIntroStage";
import { GuardrailStage } from "../stages/GuardrailStage";
import { KnowledgeBaseStage } from "../stages/KnowledgeBaseStage";
import { MakeItYoursStage } from "../stages/MakeItYoursStage";
import { MeetAgentKStage } from "../stages/MeetAgentKStage";
import { MemoryStage } from "../stages/MemoryStage";
import { RetrievalCostStage } from "../stages/RetrievalCostStage";
import { ReviewCostStage } from "../stages/ReviewCostStage";
import { ReviewFindingsStage } from "../stages/ReviewFindingsStage";
// PARKED with chapter 4, see chapters.ts.
// import { ReviewDashboardStage } from "../stages/ReviewDashboardStage";
// PARKED with chapter 4, see chapters.ts.
// import { RunReviewAgainStage } from "../stages/RunReviewAgainStage";
import { RunReviewStage } from "../stages/RunReviewStage";
import { SaveFindingsStage } from "../stages/SaveFindingsStage";
import { SeeThePrStage } from "../stages/SeeThePrStage";
import { ShareWithAgentStage } from "../stages/ShareWithAgentStage";
import type { useLabProgress } from "../../hooks/useLabProgress";
import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDefinition } from "../../types";

export function LabStageSlot({
  lab,
  session,
  progress,
  contextFeed,
  onEndLab,
}: {
  lab: LabDefinition;
  session: ReturnType<typeof useLabRunnerSession>;
  progress: ReturnType<typeof useLabProgress>;
  /** Owned by LabRunnerPage; the retrieval lesson quotes the real size of the
   *  documents the user uploaded. */
  contextFeed: LabContextFeed;
  /** Leaves the runner — the completion screen offers it as a way out. */
  onEndLab: () => void;
}) {
  // Stable identities: these are handed to child effects as dependencies, and
  // a fresh arrow per render makes any such effect re-run on every render.
  const { goToStageById, markStageComplete, currentStage } = progress;
  const goToCreateDatapack = useCallback(() => goToStageById("create-datapack"), [goToStageById]);
  const goToMeetAgentK = useCallback(() => goToStageById("meet-agentk"), [goToStageById]);
  const goToRunReview = useCallback(() => goToStageById("run-review"), [goToStageById]);
  const currentStageId = currentStage.id;
  const completeCurrentStage = useCallback(
    () => markStageComplete(currentStageId),
    [markStageComplete, currentStageId],
  );

  // Both of these are owned here, not inside the stage components, because
  // THIS component stays mounted for the whole lab run while the switch below
  // remounts whichever stage is current -- see PrReviewRunState's own doc
  // comment. For the config that is what carries Chapter 2's choice across
  // the walk to Chapter 3.
  const [reviewConfig, setReviewConfig] = useState<ReviewAgentConfig>(DEFAULT_REVIEW_AGENT_CONFIG);
  const [reviewRun, setReviewRun] = useState<PrReviewRunState | null>(null);
  // A run that reported real token usage is a finished review, which is what
  // releases run-review's gate -- and what the findings and cost stages both
  // read. Checked on usage rather than on findings, since a review that
  // genuinely found nothing still has usage.
  // MUST be stable: PrReviewDemoApp lists onRunChange in the dependency array
  // of the effect that CALLS it, and that effect hands back a freshly built
  // object every time. A new arrow per render therefore loops -- effect fires,
  // state changes, parent re-renders, new identity, effect fires again.
  const handleReviewRunChange = useCallback(
    (run: PrReviewRunState) => {
      setReviewRun(run);
      if (run.reviewUsage) markStageComplete("run-review");
    },
    [markStageComplete],
  );
  // Chapter 4's own run (a real, user-fetched PR) -- same reasoning, its own
  // independent slot rather than reusing either run above.
  const [byocReviewRun, setByocReviewRun] = useState<PrReviewRunState | null>(null);
  // Every completed run this session, for the dashboard's per-run table and
  // the completion screen -- the only place real per-review numbers live,
  // since nothing persists them.
  const sessionRuns = [reviewRun, byocReviewRun].filter(
    (run): run is PrReviewRunState => run !== null,
  );

  // Navigating back to create-datapack and switching datapacks must not
  // leave a review run against the OLD one on screen: BringYourOwnPrStage
  // would keep showing it (fetch form unreachable), and a follow-up question
  // would attach the NEW datapack_id to the OLD conversation_id. Guarded on
  // an actual change (not the initial mount), the same way
  // useLabRunnerSession's own datapackId-change eviction effect is.
  const prevDatapackIdRef = useRef(session.datapackId);
  useEffect(() => {
    if (prevDatapackIdRef.current === session.datapackId) return;
    prevDatapackIdRef.current = session.datapackId;
    setByocReviewRun(null);
  }, [session.datapackId]);

  switch (progress.currentStage.id) {
    case "see-the-pr":
      return (
        <SeeThePrStage
          agentDisplayName={lab.demoApp.agentDisplayName}
          onComplete={completeCurrentStage}
        />
      );
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
          onComplete={completeCurrentStage}
        />
      );
    case "knowledge-base":
      return (
        <KnowledgeBaseStage
          session={session}
          feed={contextFeed}
          onGoToCreateDatapack={goToCreateDatapack}
        />
      );
    case "memory":
      return <MemoryStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "conversation":
      return <ConversationStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "meet-agentk":
      return (
        <MeetAgentKStage demoApp={lab.demoApp} session={session} onGoToCreateDatapack={goToCreateDatapack} />
      );
    case "share-with-agent":
      return (
        <ShareWithAgentStage
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onComplete={completeCurrentStage}
        />
      );
    case "configure-agentk":
      return (
        <ConfigureAgentKStage
          agentDisplayName={lab.demoApp.agentDisplayName}
          datapackId={session.datapackId}
          config={reviewConfig}
          onConfigChange={setReviewConfig}
          onGoToCreateDatapack={goToCreateDatapack}
        />
      );
    case "retrieval-cost":
      return <RetrievalCostStage agentDisplayName={lab.demoApp.agentDisplayName} feed={contextFeed} />;
    case "guardrail":
      return <GuardrailStage agentDisplayName={lab.demoApp.agentDisplayName} />;
    case "run-review":
      return (
        <RunReviewStage
          demoApp={lab.demoApp}
          session={session}
          reviewConfig={reviewConfig}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToMeetAgentK}
          savedRun={reviewRun}
          onRunChange={handleReviewRunChange}
        />
      );
    case "review-findings":
      return (
        <ReviewFindingsStage
          run={reviewRun}
          datapackId={session.datapackId}
          onGoToRunReview={goToRunReview}
        />
      );
    case "review-cost":
      return (
        <ReviewCostStage
          run={reviewRun}
          datapackId={session.datapackId}
          datapackName={session.datapackName}
          onGoToRunReview={goToRunReview}
        />
      );
    case "collaborate":
      return <CollaborateStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    // case "dashboard-intro":
    // return <DashboardIntroStage />;
    // // case "create-amp-cluster":
    // //   return <CreateAmpClusterStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    // case "run-review-again":
    // return (
    // <RunReviewAgainStage
    // demoApp={lab.demoApp}
    // session={session}
    // onGoToCreateDatapack={goToCreateDatapack}
    // onGoToDeployAgent={goToMeetAgentK}
    // savedRun={dashboardReviewRun}
    // onRunChange={handleDashboardRunChange}
    // />
    // );
    // case "review-dashboard":
    // return (
    // <ReviewDashboardStage
    // session={session}
    // sessionRuns={sessionRuns}
    // onGoToCreateDatapack={goToCreateDatapack}
    // />
    // );
    case "save-findings":
      return <SaveFindingsStage session={session} run={reviewRun} onComplete={completeCurrentStage} />;
    case "make-it-yours":
      return (
        <MakeItYoursStage
          demoApp={lab.demoApp}
          session={session}
          sessionRuns={sessionRuns}
          reviewConfig={reviewConfig}
          byocRun={byocReviewRun}
          onByocRunChange={setByocReviewRun}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToMeetAgentK}
          onGoToCollaborate={() => progress.goToStageById("collaborate")}
          onEndLab={onEndLab}
        />
      );
    default:
      return null;
  }
}
