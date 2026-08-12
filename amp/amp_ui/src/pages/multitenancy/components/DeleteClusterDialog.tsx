// Type-to-confirm dialog for deleting a cluster. Unlike unprovisioning compute
// (which leaves data intact), this cascades through every branch and endpoint
// and destroys the data — so it's gated behind typing the cluster name, not a
// one-click confirm.

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useDeleteCluster } from "@/lib/api/queries";
import { dataBranchingMessages as msg } from "@/lib/messages";

export function DeleteClusterDialog({
  open,
  onOpenChange,
  poolId,
  clusterId,
  clusterName,
  onDeleted,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  clusterId: string;
  clusterName: string;
  /** Called after the cascade succeeds — the caller clears its selection. */
  onDeleted?: () => void;
}) {
  const [confirmText, setConfirmText] = useState("");
  const del = useDeleteCluster();

  // Reset the typed confirmation each time the dialog opens.
  useEffect(() => {
    if (open) setConfirmText("");
  }, [open]);

  const canDelete = confirmText.trim() === clusterName && !del.isPending;

  function remove() {
    del.mutate(
      { poolId, clusterId },
      {
        onSuccess: () => {
          toast.success(msg.deleteClusterSuccess(clusterName));
          onOpenChange(false);
          onDeleted?.();
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : msg.deleteClusterError),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !del.isPending && onOpenChange(o)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{msg.deleteClusterTitle(clusterName)}</DialogTitle>
          <DialogDescription>{msg.deleteClusterConfirm(clusterName)}</DialogDescription>
        </DialogHeader>
        <div className="my-4 space-y-2">
          <Label htmlFor="confirm-cluster-name">
            {msg.deleteClusterConfirmPrompt(clusterName)}
          </Label>
          <Input
            id="confirm-cluster-name"
            value={confirmText}
            onChange={(e) => setConfirmText(e.target.value)}
            placeholder={clusterName}
            autoComplete="off"
            autoFocus
          />
        </div>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={del.isPending}
          >
            Cancel
          </Button>
          <Button type="button" variant="destructive" onClick={remove} disabled={!canDelete}>
            {del.isPending ? "Deleting…" : "Delete cluster"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
