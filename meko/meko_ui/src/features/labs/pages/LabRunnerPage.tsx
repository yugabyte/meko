import { Loader2 } from "lucide-react";
import { Link, Navigate, useLocation } from "react-router-dom";

import { ROUTES } from "@/app/routes/paths";

import { getLabDefinition, LAB_DEFINITIONS } from "../data/labDefinitions";
import { parseLabRoute } from "../lib/parseLabRoute";
import { useLabProgress } from "../hooks/useLabProgress";
import { useLabRunnerSession } from "../hooks/useLabRunnerSession";
import { LabStageNav } from "../components/lab-runner/LabStageNav";
import { LabStageSlot } from "../components/lab-runner/LabStageSlot";
import { LabObservationSlot } from "../components/lab-runner/LabObservationSlot";

export default function LabRunnerPage() {
  const location = useLocation();
  const parsed = parseLabRoute(location.pathname);
  const lab = parsed?.isRunner ? getLabDefinition(parsed.labId) : undefined;

  // Hooks must run unconditionally — an invalid :labId means `lab` is
  // undefined, so we call these with LAB_DEFINITIONS[0] as a placeholder
  // and redirect below once we know whether the real lookup succeeded.
  const progress = useLabProgress(lab ?? LAB_DEFINITIONS[0]);
  const session = useLabRunnerSession(lab ?? LAB_DEFINITIONS[0]);

  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  // Create Datapack doesn't need the provision-status lookup the other 4
  // stages wait on, so it shouldn't block on that spinner.
  const showSpinner = session.loading && progress.currentStage.id !== "create-datapack";

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
            refreshTick={session.refreshTick}
          />
        </div>
      </div>
    </div>
  );
}
