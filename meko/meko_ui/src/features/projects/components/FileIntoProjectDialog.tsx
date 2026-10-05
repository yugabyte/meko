// File a datapack into a folder, or move one that is already filed. The
// destination picker itself is ProjectFolderSelect, shared with the
// create-datapack wizard.
//
// Two entry points, one dialog: the Datapacks page opens it for a row the
// user clicked, and the project Settings tab opens it with nothing chosen and
// a list to pick from. Placement is an upsert keyed by datapack, so filing
// and moving are the same write.

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
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Datapack } from "@/lib/api/mekoApi";
import { usePlaceDatapack } from "@/features/projects/hooks/useProjects";
import { ProjectFolderSelect } from "@/features/projects/components/ProjectFolderSelect";
import { PROJECT_TOASTS, TOAST_RETRY_DESCRIPTION } from "@/features/projects/projectsCopy";

export function FileIntoProjectDialog({
  open,
  onOpenChange,
  datapack,
  choices,
  title = "File into a project",
  confirmLabel = "File here",
  successTitle = PROJECT_TOASTS.datapackFiled.ok,
  defaultProjectId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The datapack to file. Null with `choices` set means "let the user pick". */
  datapack: Datapack | null;
  /** Offered when no datapack was passed in. Owned datapacks only: you cannot
   *  move something you do not own. */
  choices?: Datapack[];
  title?: string;
  confirmLabel?: string;
  /** Overridden by callers that open this as a move, where "filed" is the
   *  wrong past tense for a datapack that already had a home. */
  successTitle?: string;
  /** Preselects the destination project. Passed by a project's own Settings
   *  tab, where "move here" means this project. */
  defaultProjectId?: string;
}) {
  const [chosenId, setChosenId] = useState("");
  const [folderId, setFolderId] = useState("");
  const [error, setError] = useState<string | null>(null);
  const placeMut = usePlaceDatapack();

  useEffect(() => {
    if (!open) return;
    setChosenId("");
    setFolderId("");
    setError(null);
  }, [open, datapack?.datapack_id]);

  const targetId = datapack?.datapack_id ?? chosenId;

  const handleFile = async () => {
    if (!targetId || !folderId || placeMut.isPending) return;
    setError(null);
    try {
      await placeMut.mutateAsync({ datapackId: targetId, folderId });
      onOpenChange(false);
      toast({ title: successTitle });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: PROJECT_TOASTS.datapackFiled.fail,
        description: message,
      });
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void handleFile();
          }}
        >
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>
              {datapack
                ? `Choose where “${datapack.datapack_name}” should live.`
                : "Choose a datapack and where it should live."}
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            {!datapack && choices ? (
              <div className="space-y-2">
                <Label htmlFor="file-into-datapack">Datapack</Label>
                <Select value={chosenId} onValueChange={setChosenId}>
                  <SelectTrigger id="file-into-datapack">
                    <SelectValue placeholder="Select a datapack" />
                  </SelectTrigger>
                  <SelectContent>
                    {choices.map((dp) => (
                      <SelectItem key={dp.datapack_id} value={dp.datapack_id}>
                        {dp.datapack_name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            ) : null}

            <ProjectFolderSelect
              folderId={folderId}
              onFolderChange={setFolderId}
              enabled={open}
              idPrefix="file-into"
              defaultProjectId={defaultProjectId}
            />

            {error ? (
              <p role="alert" className="text-sm text-destructive">
                {error}
              </p>
            ) : null}
          </div>

          <DialogFooter>
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={!targetId || !folderId || placeMut.isPending}>
              {placeMut.isPending ? "Saving…" : confirmLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
