// Exit and Restart, the two ways out of a lab run. Both discard everything
// the run saved, so both confirm first.
// Shared: any guided runner that persists progress and needs the same pair.

import { useState } from "react";
import { LogOut, RotateCcw } from "lucide-react";

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
import { Button } from "@/components/ui/button";

type PendingAction = "exit" | "restart" | null;

const COPY = {
  exit: {
    title: "Exit the lab?",
    body: "Your progress will be lost. The datapack you built stays where it is, but this run starts over from Chapter 1 next time.",
    confirm: "Exit lab",
  },
  restart: {
    title: "Start again?",
    body: "Your progress will be lost and you'll go back to Chapter 1. The datapack you built stays where it is.",
    confirm: "Restart lab",
  },
} as const;

export function LabExitControls({
  onExit,
  onRestart,
}: {
  /** Discard the run's saved state and leave the runner. */
  onExit: () => void;
  /** Discard the run's saved state and return to the first step, in place. */
  onRestart: () => void;
}) {
  const [pending, setPending] = useState<PendingAction>(null);
  const copy = pending ? COPY[pending] : null;

  return (
    <>
      <Button variant="ghost" size="sm" onClick={() => setPending("restart")}>
        <RotateCcw className="h-3.5 w-3.5" aria-hidden />
        Restart Lab
      </Button>
      <Button variant="outline" size="sm" onClick={() => setPending("exit")}>
        <LogOut className="h-3.5 w-3.5" aria-hidden />
        Exit Lab
      </Button>

      <AlertDialog open={pending !== null} onOpenChange={(next) => !next && setPending(null)}>
        <AlertDialogContent className="border-border bg-card">
          <AlertDialogHeader>
            <AlertDialogTitle className="text-foreground">{copy?.title}</AlertDialogTitle>
            <AlertDialogDescription className="text-muted-foreground">
              {copy?.body}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              onClick={() => {
                const action = pending;
                setPending(null);
                if (action === "exit") onExit();
                if (action === "restart") onRestart();
              }}
            >
              {copy?.confirm}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </>
  );
}
