import { Loader2 } from "lucide-react";
import { Link, Navigate, useLocation } from "react-router-dom";

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

// The only 2 stages that read session.userToken (SeedContextChat,
// PrReviewDemoApp) -- Deploy Agent and Collaborate need just
// session.datapackId, which is available synchronously.
const STAGES_NEEDING_TOKEN: LabStageId[] = ["seed-context", "run-review"];

export function shouldShowProvisionStatusSpinner(loading: boolean, stageId: LabStageId): boolean {
  return loading && STAGES_NEEDING_TOKEN.includes(stageId);
}

export default function LabRunnerPage() {
  const location = useLocation();
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

  return (
    <div className="flex min-h-0 w-full flex-1 flex-col overflow-hidden">
      <div className="flex items-center gap-4 border-b border-border px-4 py-2">
        <Link to={ROUTES.labs} className="shrink-0 text-sm text-muted-foreground hover:text-foreground">
          &larr; Labs
        </Link>
        <h1 className="min-w-0 flex-1 truncate text-sm font-medium">{lab.title}</h1>
      </div>
      {session.error && (
        <p className="border-b border-border bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {session.error}
        </p>
      )}
      <div className="grid min-h-0 flex-1 grid-cols-1 grid-rows-[minmax(0,1fr)] lg:grid-cols-[340px_1fr_320px]">
        <div className="hidden min-h-0 flex-col overflow-hidden lg:flex">
          <LabStageNav stages={lab.stages} progress={progress} />
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
          />
        </div>
      </div>
    </div>
  );
}
