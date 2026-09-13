import { Loader2, LogOut } from "lucide-react";
import { Navigate, useLocation, useNavigate } from "react-router-dom";

import { Button } from "@/components/ui/button";

import { ROUTES } from "@/app/routes/paths";
import NotFoundPage from "@/features/system/pages/NotFoundPage";

import { getLabDefinition, LAB_DEFINITIONS } from "../data/labDefinitions";
import { parseLabRoute } from "../lib/parseLabRoute";
import { useLabsNavFlag } from "../hooks/useLabsNavFlag";
import { useLabProgress } from "../hooks/useLabProgress";
import { useLabRunnerSession } from "../hooks/useLabRunnerSession";
import { LabStageNav } from "../components/lab-runner/LabStageNav";
import { LabStageSlot } from "../components/lab-runner/LabStageSlot";
import { LabObservationSlot } from "../components/lab-runner/LabObservationSlot";
import type { LabStageId } from "../types";

// The only stages that read session.userToken (MemoryStage,
// ConversationStage's SeedContextChat, and both PrReviewDemoApp call sites --
// run-review and run-review-again) -- Deploy Agent and Collaborate need just
// session.datapackId, which is available synchronously.
const STAGES_NEEDING_TOKEN: LabStageId[] = ["memory", "conversation", "run-review", "run-review-again"];

export function shouldShowProvisionStatusSpinner(loading: boolean, stageId: LabStageId): boolean {
  return loading && STAGES_NEEDING_TOKEN.includes(stageId);
}

export default function LabRunnerPage() {
  const location = useLocation();
  const navigate = useNavigate();
  const parsed = parseLabRoute(location.pathname);
  const lab = parsed?.isRunner ? getLabDefinition(parsed.labId) : undefined;
  const showLabs = useLabsNavFlag();

  // Hooks must run unconditionally — an invalid :labId means `lab` is
  // undefined, so we call these with LAB_DEFINITIONS[0] as a placeholder
  // and redirect below once we know whether the real lookup succeeded.
  const progress = useLabProgress(lab ?? LAB_DEFINITIONS[0]);
  const session = useLabRunnerSession(lab ?? LAB_DEFINITIONS[0]);

  // Pre-GA: the flag gates more than the Sidebar entry -- a direct URL hit
  // must bounce too, same as BillingPage/AdminPage self-guard.
  if (!showLabs) return <NotFoundPage />;
  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  const showSpinner = shouldShowProvisionStatusSpinner(session.loading, progress.currentStage.id);

  const LabIcon = lab.icon;

  return (
    <div className="flex h-screen h-dvh w-full flex-col overflow-hidden bg-background text-foreground">
      <div className="flex items-center gap-3 border-b border-border px-4 py-3">
        <LabIcon className="h-5 w-5 shrink-0 text-primary" aria-hidden />
        <h1 className="min-w-0 flex-1 truncate text-sm font-medium">{lab.title}</h1>
        <Button variant="outline" size="sm" onClick={() => navigate(ROUTES.labs)}>
          <LogOut className="h-3.5 w-3.5" aria-hidden />
          End Lab
        </Button>
      </div>
      {session.error && (
        <p className="border-b border-border bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {session.error}
        </p>
      )}
      <div className="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,1fr)] lg:grid-cols-[340px_1fr_440px]">
        <div className="hidden min-h-0 flex-col overflow-hidden lg:flex">
          <LabStageNav chapters={lab.chapters} progress={progress} />
        </div>
        <div className="flex min-h-0 flex-col overflow-hidden">
          {showSpinner ? (
            <div className="flex flex-1 items-center justify-center">
              <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : (
            <LabStageSlot lab={lab} session={session} progress={progress} />
          )}
        </div>
        <div className="hidden min-h-0 flex-col overflow-hidden lg:flex">
          <LabObservationSlot
            observation={lab.observation}
            datapackId={session.datapackId}
            datapackName={session.datapackName}
            datapackGrant={session.datapackGrant}
            refreshTick={session.refreshTick}
            lastLessonTrace={session.lastLessonTrace}
          />
        </div>
      </div>
    </div>
  );
}
