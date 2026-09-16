// Add externalized extensions to a branch. Add-only: the API unions names into
// the branch's set and has no removal path, so what is already enabled renders
// locked. The task redeploys every endpoint on the branch, which drops their
// connections — said up front, before the user commits.

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
import { ExtensionsField, useExtensionOptions } from "@/components/ui/extensions-field";
import { useAddBranchExtensions } from "@/lib/api/queries";
import { dataBranchingMessages as msg } from "@/lib/messages";

export function ExtensionsDialog({
  open,
  onOpenChange,
  poolId,
  clusterId,
  branchId,
  enabled,
  title,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  clusterId: string;
  branchId: string;
  enabled: string[];
  title: string;
}) {
  const [picked, setPicked] = useState<string[]>([]);
  const add = useAddBranchExtensions();
  // Live catalog, so an image an admin registered is selectable here.
  const extCatalog = useExtensionOptions();

  useEffect(() => {
    if (open) setPicked([]);
  }, [open]);

  function save() {
    add.mutate(
      { poolId, clusterId, branchId, extensions: picked },
      {
        onSuccess: () => {
          toast.success(msg.extensionsAdded(picked.join(", ")));
          onOpenChange(false);
        },
        onError: (err) => toast.error(err instanceof Error ? err.message : msg.extensionsError),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={(o) => !add.isPending && onOpenChange(o)}>
      <DialogContent className="sm:max-w-xl">
        <DialogHeader>
          <DialogTitle>{msg.extensionsTitle(title)}</DialogTitle>
          <DialogDescription>{msg.extensionsDescription}</DialogDescription>
        </DialogHeader>

        <ExtensionsField
          value={picked}
          onChange={setPicked}
          enabled={enabled}
          options={extCatalog.options}
        />

        <p className="text-xs text-muted-foreground">{msg.extensionsRedeployWarning}</p>

        <DialogFooter className="gap-2 sm:justify-between">
          <Button
            type="button"
            variant="ghost"
            onClick={() => onOpenChange(false)}
            disabled={add.isPending}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="brand"
            onClick={save}
            disabled={picked.length === 0 || add.isPending}
          >
            {add.isPending ? "Adding…" : "Add extensions"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
