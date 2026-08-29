// Stage 2: always-visible, open-ended context seeding — file upload AND a
// free-form chat, both visible at once (not a toggle). Replaces the old
// triggered "Load coding standards" dialog for one bundled fixture.

import FileUpload from "@/features/datapacks/components/FileUpload";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { SeedContextChat } from "./SeedContextChat";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

export function SeedContextStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <FileUpload
        datapackName={session.datapackName ?? ""}
        datapackId={session.datapackId}
        onKnowledgeBaseCreated={session.bumpRefreshTick}
      />
      <div className="flex items-center gap-3">
        <div className="h-px flex-1 bg-border" />
        <span className="text-xs uppercase tracking-wide text-muted-foreground">
          or, tell Meko what to remember
        </span>
        <div className="h-px flex-1 bg-border" />
      </div>
      <SeedContextChat
        datapackId={session.datapackId}
        userId={session.userId}
        userToken={session.userToken}
        onTurnComplete={session.bumpRefreshTick}
      />
    </div>
  );
}
