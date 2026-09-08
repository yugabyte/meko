import { Button } from "@/components/ui/button";

/** Shared degrade prompt for every stage that needs a datapack already
 *  selected — CreateDatapackStage is the only stage that sets the
 *  selection, so every other stage degrades to this until it does. */
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
