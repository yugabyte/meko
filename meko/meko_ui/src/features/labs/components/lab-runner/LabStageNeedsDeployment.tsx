import { Button } from "@/components/ui/button";

/** Degrade prompt for Run a Review: it invokes the deployed agent directly
 *  (no fallback to a direct chat path), so it needs an active deployment
 *  before it can do anything. Mirrors LabStageNeedsDatapack.tsx's pattern. */
export function LabStageNeedsDeployment({
  onGoToDeployAgent,
  deploymentFailed,
}: {
  onGoToDeployAgent: () => void;
  deploymentFailed?: boolean;
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="text-sm font-medium">
        {deploymentFailed ? "Deployment failed" : "Deploy your agent first"}
      </p>
      <p className="max-w-xs text-sm text-muted-foreground">
        This step runs against your deployed agent. There's no fallback to a direct chat.
      </p>
      <Button size="sm" variant="outline" onClick={onGoToDeployAgent}>
        Go to Deploy
      </Button>
    </div>
  );
}
