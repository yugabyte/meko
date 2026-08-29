import { Button } from "@/components/ui/button";

/** Shared degrade prompt for the 4 stages that need a datapack already
 *  selected. Same copy/pattern the page-level "Choose a datapack to begin"
 *  placeholder used before the restructuring — now reused per-stage since
 *  CreateDatapackStage is what actually sets the selection. */
export function LabStageNeedsDatapack({ onGoToCreateDatapack }: { onGoToCreateDatapack: () => void }) {
  return (
    <div className="flex flex-1 flex-col items-center justify-center gap-2 p-6 text-center">
      <p className="text-sm font-medium">Choose or create a datapack first</p>
      <p className="max-w-xs text-sm text-muted-foreground">
        This step reads and writes into this lab's datapack.
      </p>
      <Button size="sm" variant="outline" onClick={onGoToCreateDatapack}>
        Go to Create Datapack
      </Button>
    </div>
  );
}
