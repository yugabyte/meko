import { CollaborateStage } from "../stages/CollaborateStage";
import { CreateDatapackStage } from "../stages/CreateDatapackStage";
import { DeployAgentStage } from "../stages/DeployAgentStage";
import { RunReviewStage } from "../stages/RunReviewStage";
import { SeedContextStage } from "../stages/SeedContextStage";
import type { useLabProgress } from "../../hooks/useLabProgress";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDefinition } from "../../types";

/**
 * Main content pane of the lab runner: switches on progress.currentStage.id
 * (closed union, same convention the old LabDemoAppSlot used for
 * demoApp.kind) to render one of the 5 fixed stage components.
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
  const goToCreateDatapack = () => progress.goToStage(0);
  const goToDeployAgent = () => progress.goToStage(2);

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
    case "seed-context":
      return <SeedContextStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    case "deploy-agent":
      return (
        <DeployAgentStage demoApp={lab.demoApp} session={session} onGoToCreateDatapack={goToCreateDatapack} />
      );
    case "run-review":
      return (
        <RunReviewStage
          demoApp={lab.demoApp}
          session={session}
          onGoToCreateDatapack={goToCreateDatapack}
          onGoToDeployAgent={goToDeployAgent}
        />
      );
    case "collaborate":
      return <CollaborateStage session={session} onGoToCreateDatapack={goToCreateDatapack} />;
    default:
      return null;
  }
}
