// Confirmation modal shown when the user picks a different datapack from the
// header dropdown after they've already exchanged at least one chat turn.
// Switching wipes the in-process session (messages, conversation_id) and
// starts a fresh MCP conversation against the new datapack — that's not
// destructive (the old conversation is persisted in Langfuse), but it IS
// non-resumable from the UI today, so we ask first.

import { ArrowRightLeft } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";

export type WorkbenchLMSwitchConfirmDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Datapack the user is currently chatting in. */
  currentDatapackName: string;
  /** Datapack the user is trying to switch to. */
  targetDatapackName: string;
  /** Fired only when the user clicks the confirm button. */
  onConfirm: () => void;
};

export function WorkbenchLMSwitchConfirmDialog({
  open,
  onOpenChange,
  currentDatapackName,
  targetDatapackName,
  onConfirm,
}: WorkbenchLMSwitchConfirmDialogProps) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <DialogHeader>
          <DialogTitle className="flex items-center gap-2 text-xl text-foreground">
            <ArrowRightLeft className="h-5 w-5 text-blue-400" aria-hidden />
            Switch datapack?
          </DialogTitle>
          <DialogDescription className="text-muted-foreground">
            Switching to <span className="font-medium text-foreground">{targetDatapackName}</span> will end your
            current chat session with{" "}
            <span className="font-medium text-foreground">{currentDatapackName}</span>. The in-flight conversation
            won't be resumable from WorkbenchLM after the switch (it stays available in Observe).
          </DialogDescription>
        </DialogHeader>
        <DialogFooter className="gap-2 sm:gap-0">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button
            type="button"
            onClick={onConfirm}
            className="bg-blue-600 hover:bg-blue-700 text-white"
          >
            Switch datapack
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
