// Polling status display for a deployment in progress or resolved.
// useDeploymentStatus stops polling once the deployment reaches a terminal
// status (active/failed/deleted).

import { AlertCircle, CheckCircle2, Loader2 } from "lucide-react";

import { useDeploymentStatus } from "../../../hooks/useDeployments";

const STATUS_LABEL: Record<string, string> = {
  pending: "Preparing...",
  deploying: "Deploying...",
  active: "Active",
  failed: "Failed",
  deleting: "Deleting...",
  deleted: "Deleted",
};

export function DeploymentStatus({ deploymentId }: { deploymentId: string }) {
  const { data: deployment } = useDeploymentStatus(deploymentId);
  const status = deployment?.status ?? "pending";
  const label = STATUS_LABEL[status] ?? status;

  return (
    <div className="flex flex-col gap-2 rounded-md border border-border p-4">
      <div className="flex items-center gap-2 text-sm font-medium">
        {status === "active" ? (
          <CheckCircle2 className="h-4 w-4 text-emerald-600 dark:text-emerald-400" />
        ) : status === "failed" ? (
          <AlertCircle className="h-4 w-4 text-destructive" />
        ) : (
          <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />
        )}
        {label}
      </div>
      {deployment?.agent_runtime_arn && (
        <p className="break-all text-xs text-muted-foreground">{deployment.agent_runtime_arn}</p>
      )}
      {status === "failed" && deployment?.status_message && (
        <p className="text-sm text-destructive">{deployment.status_message}</p>
      )}
    </div>
  );
}
