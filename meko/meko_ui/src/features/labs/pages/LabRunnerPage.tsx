import { useEffect, useRef, useState } from "react";
import { Loader2 } from "lucide-react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { cn } from "@/lib/utils";

import { ROUTES } from "@/app/routes/paths";

import { getLabDefinition } from "../data/labDefinitions";
import { parseLabRoute } from "../lib/parseLabRoute";
import { resetLabState } from "../lib/labSessionReset";
import { LabExitControls } from "../components/lab-runner/LabExitControls";
import { useLabProgress } from "../hooks/useLabProgress";
import { useLabRunnerSession } from "../hooks/useLabRunnerSession";
import { useLabContextFeed } from "../hooks/useLabContextFeed";
import { stagesSatisfiedByContext } from "../lib/labGates";
import { LabStageNav } from "../components/lab-runner/LabStageNav";
import { LabStageSlot } from "../components/lab-runner/LabStageSlot";
import { LabStageFooter } from "../components/lab-runner/LabStageFooter";
import { LabContextPane } from "../components/lab-runner/LabContextPane";
import type { LabDefinition, LabStageId } from "../types";

// The only stages that read session.userToken: MemoryStage, ConversationStage's
// SeedContextChat, and the two stages that render PrReviewDemoApp -- run-review,
// and make-it-yours, which hosts the bring-your-own-PR review. Meet AgentK and
// Collaborate need just session.datapackId, which is available synchronously.
// Miss one and that stage renders mid-provision: deploymentActive is still
// false, so it shows a spurious "deploy the agent first" prompt.
const STAGES_NEEDING_TOKEN: LabStageId[] = [
  "memory",
  "conversation",
  "run-review",
  "make-it-yours",
];

/** The opening stage runs the agent with no context on purpose, so the pane
 *  beside it must not already be full of it -- which it would be for anyone
 *  resuming a run with a datapack still selected. */
const STAGES_BEFORE_ANY_CONTEXT: LabStageId[] = ["see-the-pr"];

/** Stages whose content needs more room than a comfortable reading measure.
 *  Both show a four-up stat grid, which wraps every label onto two lines at
 *  the default width. Prose stages stay narrow: a wide column is worse for
 *  reading, which is why this is a list and not the default. */
const WIDE_STAGES: LabStageId[] = ["review-cost", "make-it-yours"];

/** The lessons that satisfy their gate by writing into the datapack. Each
 *  reports a lesson trace on success, which releases the gate immediately.
 *  That alone isn't enough, though: a user whose datapack ALREADY holds what
 *  the lesson produces (an earlier run, a shared datapack, a reload
 *  mid-lesson) never fires the event and would be stuck with no way to
 *  satisfy it short of doing the work twice. So observed state releases the
 *  gate too -- see the effect below. */
const TRACE_COMPLETED_STAGES: LabStageId[] = ["knowledge-base", "memory", "conversation"];

export function shouldShowProvisionStatusSpinner(loading: boolean, stageId: LabStageId): boolean {
  return loading && STAGES_NEEDING_TOKEN.includes(stageId);
}

export default function LabRunnerPage() {
  const location = useLocation();
  const navigate = useNavigate();
  const parsed = parseLabRoute(location.pathname);
  const lab = parsed?.isRunner ? getLabDefinition(parsed.labId) : undefined;

  // Bumping this remounts the whole run, which is what makes Restart
  // complete: every piece of in-memory state below lives in a hook that
  // seeds itself from storage on mount, so clearing storage and remounting
  // resets all of it without each hook needing its own reset path.
  const [runKey, setRunKey] = useState(0);

  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  return (
    <LabRunnerView
      key={runKey}
      lab={lab}
      onExit={() => {
        resetLabState(lab.id);
        navigate(ROUTES.labs);
      }}
      onLeave={() => navigate(ROUTES.labs)}
      onRestart={() => {
        resetLabState(lab.id);
        setRunKey((k) => k + 1);
      }}
    />
  );
}

function LabRunnerView({
  lab,
  onExit,
  onLeave,
  onRestart,
}: {
  lab: LabDefinition;
  /** Discards the run's saved state, behind a confirm. */
  onExit: () => void;
  /** Leaves the runner with the run intact, so it resumes where it stopped.
   *  The completion screen's own "Back to Labs" -- finishing a lab is not a
   *  reason to throw it away, and it asks nothing before navigating. */
  onLeave: () => void;
  onRestart: () => void;
}) {
  const progress = useLabProgress(lab);
  const session = useLabRunnerSession(lab);

  // A lesson that recorded a trace did its work, so that's what satisfies
  // its gate. Compared by identity, not content: re-recording the same
  // lesson produces a new object, and an unchanged one must not re-fire.
  const seenTraceRef = useRef(session.lastLessonTrace);
  const { markStageComplete, currentStage } = progress;

  // Lifted out of LabContextPane so the gate and the pane read one fetch
  // rather than two: what the pane displays IS the evidence the lesson's
  // work exists.
  const contextFeed = useLabContextFeed({
    datapackId: session.datapackId,
    refreshTick: session.refreshTick,
    paused: false,
  });

  // Whatever the datapack already holds satisfies the matching lesson's gate,
  // however it got there. The lesson still renders -- this only decides
  // whether Next is blocked.
  const satisfiedByContext = stagesSatisfiedByContext(contextFeed, session.datapackId).join(",");
  useEffect(() => {
    for (const stageId of satisfiedByContext.split(",").filter(Boolean)) {
      markStageComplete(stageId as LabStageId);
    }
  }, [satisfiedByContext, markStageComplete]);

  useEffect(() => {
    const trace = session.lastLessonTrace;
    if (!trace || trace === seenTraceRef.current) return;
    seenTraceRef.current = trace;
    if (TRACE_COMPLETED_STAGES.includes(currentStage.id)) markStageComplete(currentStage.id);
  }, [session.lastLessonTrace, currentStage.id, markStageComplete]);

  const showSpinner = shouldShowProvisionStatusSpinner(session.loading, progress.currentStage.id);

  const LabIcon = lab.icon;

  return (
    <div className="flex h-screen h-dvh w-full flex-col overflow-hidden bg-background text-foreground">
      <div className="flex items-center gap-3 border-b border-border px-4 py-3">
        <LabIcon className="h-5 w-5 shrink-0 text-primary" aria-hidden />
        <div className="min-w-0 flex-1">
          <h1 className="truncate text-sm font-medium">{lab.title}</h1>
          <p className="truncate text-xs text-muted-foreground">
            Chapter {progress.chapterIndex + 1} · {progress.currentChapter.title} ·{" "}
            {progress.currentStage.title}
          </p>
        </div>
        <LabExitControls onExit={onExit} onRestart={onRestart} />
      </div>
      {session.error && (
        <p className="border-b border-border bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {session.error}
        </p>
      )}
      <div className="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,1fr)] lg:grid-cols-[300px_1fr_400px]">
        <div className="hidden min-h-0 flex-col overflow-hidden lg:flex">
          <LabStageNav chapters={lab.chapters} progress={progress} />
        </div>
        <div className="flex min-h-0 flex-col overflow-hidden">
          {showSpinner ? (
            <div className="flex flex-1 items-center justify-center">
              <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : (
            // One measure for the whole stage column, so a stage's prose and
            // its cards line up instead of the text stopping short of them.
            // Stages keep their own scrolling; this only bounds the width.
            <div
              className={cn(
                "mx-auto flex w-full min-h-0 flex-1 flex-col overflow-hidden",
                WIDE_STAGES.includes(progress.currentStage.id) ? "max-w-5xl" : "max-w-3xl",
              )}
            >
              <LabStageSlot
                lab={lab}
                session={session}
                progress={progress}
                contextFeed={contextFeed}
                onEndLab={onLeave}
              />
            </div>
          )}
          <LabStageFooter
            onBack={progress.goToPrevious}
            onNext={progress.goToNext}
            canGoBack={progress.flatIndex > 0}
            isLastStage={progress.isLastStage}
            gateMessage={progress.gateMessage}
          />
        </div>
        <div className="hidden min-h-0 flex-col overflow-hidden lg:flex">
          <LabContextPane
            feed={contextFeed}
            datapackId={session.datapackId}
            datapackName={session.datapackName}
            datapackGrant={session.datapackGrant}
            lastLessonTrace={session.lastLessonTrace}
            showContext={!STAGES_BEFORE_ANY_CONTEXT.includes(progress.currentStage.id)}
          />
        </div>
      </div>
    </div>
  );
}
