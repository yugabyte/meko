import { useState } from "react";
import { Link, Navigate, useLocation } from "react-router-dom";
import { Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { ROUTES } from "@/app/routes/paths";
import { DatapackKnowledgeUploadDialog } from "@/features/datapacks/components/datapack-detail/dialogs";

import { getLabDefinition, LAB_DEFINITIONS } from "../data/labDefinitions";
import { buildOrgCodingStandardsFile } from "../data/adlcReviewAgentLab/orgCodingStandards";
import { parseLabRoute } from "../lib/parseLabRoute";
import { useLabProgress } from "../hooks/useLabProgress";
import { useLabRunnerSession } from "../hooks/useLabRunnerSession";
import { LabStepsRail } from "../components/lab-runner/LabStepsRail";
import { LabDemoAppSlot } from "../components/lab-runner/LabDemoAppSlot";
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
  const [uploadOpen, setUploadOpen] = useState(false);

  if (!lab) return <Navigate to={ROUTES.labs} replace />;

  const showSeedKnowledgeButton =
    progress.currentStep.onEnterAction === "seedKnowledge" && !session.hasOrgStandards;

  return (
    <div className="flex h-full min-h-0 w-full flex-col">
      <div className="flex items-center justify-between border-b border-border px-4 py-2">
        <Link to={ROUTES.labs} className="text-sm text-muted-foreground hover:text-foreground">
          &larr; Labs
        </Link>
        <h1 className="text-sm font-medium">{lab.title}</h1>
        <div />
      </div>
      {session.error && (
        <p className="border-b border-border bg-destructive/10 px-4 py-2 text-sm text-destructive">
          {session.error}
        </p>
      )}
      <div className="grid min-h-0 flex-1 grid-cols-1 lg:grid-cols-[280px_1fr_320px]">
        <div className="hidden lg:block">
          <LabStepsRail lab={lab} progress={progress} />
        </div>
        <div className="flex min-h-0 flex-col overflow-hidden">
          {showSeedKnowledgeButton && (
            <div className="border-b border-border p-3">
              <Button
                size="sm"
                onClick={() => setUploadOpen(true)}
                disabled={!session.datapackId}
              >
                Load coding standards into this lab
              </Button>
            </div>
          )}
          {session.loading ? (
            <div className="flex flex-1 items-center justify-center">
              <Loader2 className="h-5 w-5 animate-spin text-muted-foreground" />
            </div>
          ) : (
            <LabDemoAppSlot demoApp={lab.demoApp} session={session} />
          )}
        </div>
        <div className="hidden lg:block">
          <LabObservationSlot
            observation={lab.observation}
            datapackId={session.datapackId}
            datapackName={session.datapackName}
            refreshTick={session.refreshTick}
          />
        </div>
      </div>
      {session.datapackId && session.datapackName && (
        <DatapackKnowledgeUploadDialog
          datapackId={session.datapackId}
          datapackName={session.datapackName}
          open={uploadOpen}
          onOpenChange={(open) => {
            setUploadOpen(open);
            if (!open) void session.refreshKnowledgeStatus();
          }}
          initialFiles={[buildOrgCodingStandardsFile()]}
        />
      )}
    </div>
  );
}
