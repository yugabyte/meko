// Destructive confirm dialog that unlocks the action only once the user types an
// exact phrase (usually the resource name) — the guard for irreversible,
// cascading deletes. Reusable across meko destructive flows.
import { useEffect, useState, type ReactNode } from "react";
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

export type ConfirmByTypingDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  description?: ReactNode;
  /** The exact text the user must type before the destructive action unlocks. */
  confirmPhrase: string;
  confirmLabel?: string;
  pending?: boolean;
  onConfirm: () => void | Promise<void>;
  /** Rendered above the confirm input, for flows that must choose WHICH
   *  resource to destroy before naming it. Leave it out and the dialog is
   *  exactly what it was. */
  children?: ReactNode;
};

export function ConfirmByTypingDialog({
  open,
  onOpenChange,
  title,
  description,
  confirmPhrase,
  confirmLabel = "Delete",
  pending = false,
  onConfirm,
  children,
}: ConfirmByTypingDialogProps) {
  const [value, setValue] = useState("");
  useEffect(() => {
    if (open) setValue("");
  }, [open]);
  // An empty phrase can never be confirmed: the caller has not chosen a
  // target yet, so there is nothing to name.
  const matches = confirmPhrase !== "" && value.trim() === confirmPhrase;

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!pending) onOpenChange(o);
      }}
    >
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="text-foreground">{title}</DialogTitle>
          {description ? (
            <DialogDescription className="text-muted-foreground">{description}</DialogDescription>
          ) : null}
        </DialogHeader>
        {children ? <div className="mt-2 space-y-2">{children}</div> : null}
        <div className="my-2">
          <Input
            autoFocus
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={confirmPhrase}
            aria-label="Type to confirm"
            onKeyDown={(e) => {
              if (e.key === "Enter" && matches && !pending) void onConfirm();
            }}
          />
        </div>
        <DialogFooter className="gap-2 sm:gap-0">
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={pending}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={() => void onConfirm()}
            disabled={!matches || pending}
          >
            {pending ? "Deleting…" : confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
