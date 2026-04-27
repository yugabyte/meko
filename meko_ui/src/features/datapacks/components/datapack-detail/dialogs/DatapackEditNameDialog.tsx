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
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";

export type DatapackEditSavePayload = {
  name: string;
  description: string;
};

export type DatapackEditNameDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentName: string;
  currentDescription: string;
  onSave: (payload: DatapackEditSavePayload) => Promise<void>;
};

export function DatapackEditNameDialog({
  open,
  onOpenChange,
  currentName,
  currentDescription,
  onSave,
}: DatapackEditNameDialogProps) {
  const [nameValue, setNameValue] = useState(currentName);
  const [descriptionValue, setDescriptionValue] = useState(currentDescription);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setNameValue(currentName);
      setDescriptionValue(currentDescription);
      setError(null);
      setSubmitting(false);
    }
  }, [open, currentName, currentDescription]);

  const handleSave = async () => {
    const nextName = nameValue.trim();
    if (!nextName) {
      setError("Name is required.");
      return;
    }
    const nextDesc = descriptionValue;
    const prevDesc = currentDescription;
    if (nextName === currentName && nextDesc.trim() === prevDesc.trim()) {
      onOpenChange(false);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await onSave({ name: nextName, description: nextDesc });
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not update datapack.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-[#1C2333] border-border text-foreground max-h-[calc(100vh-4rem)] overflow-y-auto">
        <DialogHeader>
          <DialogTitle className="text-xl text-white">Edit datapack</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Update the name and description for{" "}
            <span className="text-foreground font-medium">{currentName}</span>.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-2">
          <div className="space-y-2">
            <Label htmlFor="datapack-edit-name" className="text-muted-foreground">
              Name
            </Label>
            <Input
              id="datapack-edit-name"
              value={nameValue}
              onChange={(e) => setNameValue(e.target.value)}
              className="bg-[#0E1525] border-border"
              autoComplete="off"
              disabled={submitting}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="datapack-edit-description" className="text-muted-foreground">
              Description
            </Label>
            <Textarea
              id="datapack-edit-description"
              value={descriptionValue}
              onChange={(e) => setDescriptionValue(e.target.value)}
              className="min-h-[100px] resize-y bg-[#0E1525] border-border text-sm"
              placeholder="Optional — shown where this datapack is summarized."
              disabled={submitting}
            />
          </div>
          {error ? (
            <p className="text-sm text-red-300" role="alert">
              {error}
            </p>
          ) : null}
        </div>
        <DialogFooter className="gap-2 sm:gap-0">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
            Cancel
          </Button>
          <Button type="button" onClick={() => void handleSave()} disabled={submitting}>
            {submitting ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
