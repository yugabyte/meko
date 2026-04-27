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

export type DatapackEditNameDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  currentName: string;
  onRename: (newName: string) => Promise<void>;
};

export function DatapackEditNameDialog({ open, onOpenChange, currentName, onRename }: DatapackEditNameDialogProps) {
  const [value, setValue] = useState(currentName);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setValue(currentName);
      setError(null);
      setSubmitting(false);
    }
  }, [open, currentName]);

  const handleSave = async () => {
    const next = value.trim();
    if (!next) {
      setError("Name is required.");
      return;
    }
    if (next === currentName) {
      onOpenChange(false);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await onRename(next);
      onOpenChange(false);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Could not rename datapack.");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-[#1C2333] border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="text-xl text-white">Rename datapack</DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Update the current name for the following datapack: <span className="text-foreground font-medium">{currentName}</span>.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-2 py-2">
          <Label htmlFor="datapack-rename-input" className="text-muted-foreground">
            New name
          </Label>
          <Input
            id="datapack-rename-input"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            className="bg-[#0E1525] border-border"
            autoComplete="off"
            disabled={submitting}
          />
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
