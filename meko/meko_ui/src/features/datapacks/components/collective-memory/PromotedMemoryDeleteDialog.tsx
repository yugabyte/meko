// Confirm dialog for deleting a promoted (collective) memory.
// Extracted from CollectiveMemoryPanel so the panel focuses on list/render.

import { Loader2, Trash2 } from "lucide-react";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { buttonVariants } from "@/components/ui/button";
import type { PromotedMemory } from "@/lib/api/mekoApi";
import { cn } from "@/lib/utils";

type Props = {
  /** Memory pending deletion. `null` closes the dialog. */
  memory: PromotedMemory | null;
  /** True while the underlying delete request is in flight — disables both buttons and shows the spinner. */
  deleting: boolean;
  /** Fires when the user cancels (either the Cancel button or a click-outside). Ignored while `deleting`. */
  onCancel: () => void;
  /** Fires when the user confirms. Parent owns the actual delete call and the close-on-success. */
  onConfirm: (memoryId: string) => void;
};

export function PromotedMemoryDeleteDialog({ memory, deleting, onCancel, onConfirm }: Props) {
  return (
    <AlertDialog
      open={memory !== null}
      onOpenChange={(next) => {
        if (!next && !deleting) onCancel();
      }}
    >
      <AlertDialogContent className="bg-card border-border">
        <AlertDialogHeader>
          <AlertDialogTitle className="text-foreground">
            Delete this collective memory?
          </AlertDialogTitle>
          <AlertDialogDescription className="text-muted-foreground">
            This removes it from the datapack&apos;s shared knowledge and RAG search will no longer
            return it.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {memory ? (
          <blockquote
            className="rounded-md border border-border bg-card px-3 py-2 text-sm italic text-foreground/90"
            title={memory.data}
          >
            &ldquo;{memory.data}&rdquo;
          </blockquote>
        ) : null}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={deleting}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={(e) => {
              // Keep dialog open until delete resolves; parent clears `memory` on success.
              e.preventDefault();
              if (memory) onConfirm(memory.id);
            }}
            disabled={deleting}
            className={cn(buttonVariants({ variant: "destructive" }), "gap-1.5")}
          >
            {deleting ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
            ) : (
              <Trash2 className="h-3.5 w-3.5" aria-hidden />
            )}
            Delete
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}
