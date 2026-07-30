// Confirmation modal shown when the datapack owner / maintainer picks a
// new role from a member-row dropdown. Mirrors the shape of
// `DatapackDeleteConfirmDialog` so role-change, revoke, and delete all
// feel like the same family of confirmations.

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

export type DatapackChangeRoleConfirmDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  memberName: string;
  datapackName: string;
  fromRoleLabel: string;
  toRoleLabel: string;
  onConfirm: () => Promise<void>;
};

export function DatapackChangeRoleConfirmDialog({
  open,
  onOpenChange,
  memberName,
  datapackName,
  fromRoleLabel,
  toRoleLabel,
  onConfirm,
}: DatapackChangeRoleConfirmDialogProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setError(null);
      setSubmitting(false);
    }
  }, [open]);

  const handleConfirm = async () => {
    setSubmitting(true);
    setError(null);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not update role.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="text-xl text-foreground">Update member role</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Are you sure you want to update{" "}
            <span className="font-medium text-foreground">{memberName}</span>'s role from{" "}
            <span className="font-medium text-foreground">{fromRoleLabel}</span> to{" "}
            <span className="font-medium text-foreground">{toRoleLabel}</span> for datapack{" "}
            <span className="font-medium text-foreground">{datapackName}</span>?
          </DialogDescription>
        </DialogHeader>
        {error ? (
          <p className="text-sm text-red-700 dark:text-red-300" role="alert">
            {error}
          </p>
        ) : null}
        <DialogFooter className="gap-2 sm:gap-0">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
            Cancel
          </Button>
          <Button type="button" onClick={() => void handleConfirm()} disabled={submitting}>
            {submitting ? "Updating…" : "Update role"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
