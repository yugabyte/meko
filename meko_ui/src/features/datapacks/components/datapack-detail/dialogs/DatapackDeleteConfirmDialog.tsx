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

export type DatapackDeleteConfirmDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackName: string;
  onConfirm: () => Promise<void>;
};

export function DatapackDeleteConfirmDialog({
  open,
  onOpenChange,
  datapackName,
  onConfirm,
}: DatapackDeleteConfirmDialogProps) {
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setError(null);
      setSubmitting(false);
    }
  }, [open]);

  const handleDelete = async () => {
    setSubmitting(true);
    setError(null);
    try {
      await onConfirm();
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not delete datapack.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-[#1C2333] border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="text-xl text-white">Delete datapack</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Are you sure you want to delete{" "}
            <span className="font-medium text-foreground">{datapackName}</span>? This cannot be undone and may remove
            associated knowledge and access for this workspace.
          </DialogDescription>
        </DialogHeader>
        {error ? (
          <p className="text-sm text-red-300" role="alert">
            {error}
          </p>
        ) : null}
        <DialogFooter className="gap-2 sm:gap-0">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
            Cancel
          </Button>
          <Button type="button" variant="destructive" onClick={() => void handleDelete()} disabled={submitting}>
            {submitting ? "Deleting…" : "Delete"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
