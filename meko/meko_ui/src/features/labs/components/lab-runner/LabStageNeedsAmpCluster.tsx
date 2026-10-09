import { Button } from "@/components/ui/button";

/** Degrade prompt for the dashboard stage. The dashboard itself renders
 *  sample data unconditionally today (see ReviewDashboardStage's own
 *  comment) -- this gate isn't a technical dependency on the cluster, it's
 *  the chapter's own sequencing: provisioning a cluster is the point of the
 *  previous stage, so this stage shouldn't be reachable without having done
 *  it. Mirrors LabStageNeedsDatapack.tsx/LabStageNeedsDeployment.tsx's
 *  pattern. */
export function LabStageNeedsAmpCluster({
  onGoToCreateAmpCluster,
}: {
  onGoToCreateAmpCluster: () => void;
}) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="text-sm font-medium">Create an AMP cluster first</p>
      <p className="max-w-xs text-sm text-muted-foreground">
        This dashboard previews what review-run tracking will look like once it's backed by a real
        AMP cluster. Create one to continue.
      </p>
      <Button size="sm" variant="outline" onClick={onGoToCreateAmpCluster}>
        Go to Create an AMP cluster
      </Button>
    </div>
  );
}
