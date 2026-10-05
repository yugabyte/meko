// Add a folder to this project, or rename one of its folders.
//
// One dialog for both: they differ only in whether an existing folder has to
// be chosen first, and keeping them together means the name rules and the
// error handling have one implementation.

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
import { Textarea } from "@/components/ui/textarea";
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
import type { ProjectFolder } from "@/lib/api/projectsApi";
import { useCreateFolder, useRenameFolder } from "@/features/projects/hooks/useProjects";
import {
  ADD_FOLDER_LABEL,
  PROJECT_TOASTS,
  FOLDER_DESCRIPTION_HINT,
  RENAME_FOLDER_LABEL,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/projects/projectsCopy";

export function FolderNameDialog({
  open,
  onOpenChange,
  projectId,
  folders,
  mode,
  initialFolderId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  projectId: string;
  /** Only read in rename mode, to choose which folder to act on. */
  folders: ProjectFolder[];
  mode: "create" | "rename";
  /** Preselects the folder to rename, so the pencil on a row opens the
   *  dialog already pointed at it instead of asking again. */
  initialFolderId?: string;
}) {
  const [folderId, setFolderId] = useState("");
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [error, setError] = useState<string | null>(null);
  const createMut = useCreateFolder(projectId);
  const renameMut = useRenameFolder(projectId);
  const pending = createMut.isPending || renameMut.isPending;

  useEffect(() => {
    if (!open) return;
    setFolderId(mode === "rename" ? (initialFolderId ?? "") : "");
    setName("");
    setDescription("");
    setError(null);
  }, [open, mode, initialFolderId]);

  // Renaming starts from the current name, which is what the user is editing.
  useEffect(() => {
    if (mode !== "rename" || !folderId) return;
    const existing = folders.find((f) => f.folder_id === folderId);
    setName(existing?.folder_name ?? "");
    setDescription(existing?.folder_description ?? "");
  }, [mode, folderId, folders]);

  const trimmed = name.trim();
  const ready = trimmed !== "" && (mode === "create" || folderId !== "");

  const handleSubmit = async () => {
    if (!ready || pending) return;
    // The dialog closes on success, so the toast is the only thing left
    // saying the folder was created; the inline error covers the failure
    // case for a user still looking at the form.
    const copy = mode === "create" ? PROJECT_TOASTS.folderAdded : PROJECT_TOASTS.folderRenamed;
    setError(null);
    try {
      if (mode === "create") {
        await createMut.mutateAsync({
          folder_name: trimmed,
          folder_description: description.trim(),
        });
      } else {
        await renameMut.mutateAsync({
          folderId,
          folderName: trimmed,
          description: description.trim(),
        });
      }
      onOpenChange(false);
      toast({ title: copy.ok, description: trimmed });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({ variant: "destructive", title: copy.fail, description: message });
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void handleSubmit();
          }}
        >
          <DialogHeader>
            <DialogTitle>{mode === "create" ? ADD_FOLDER_LABEL : RENAME_FOLDER_LABEL}</DialogTitle>
            <DialogDescription>
              {mode === "create"
                ? "Folders group the datapacks doing similar work. They do not nest."
                : "Datapacks inside keep their place."}
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            {mode === "rename" ? (
              <div className="space-y-2">
                <Label htmlFor="folder-dialog-existing">Folder</Label>
                <Select value={folderId} onValueChange={setFolderId}>
                  <SelectTrigger id="folder-dialog-existing">
                    <SelectValue placeholder="Select a folder" />
                  </SelectTrigger>
                  <SelectContent>
                    {folders.map((f) => (
                      <SelectItem key={f.folder_id} value={f.folder_id}>
                        {f.folder_name}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </div>
            ) : null}

            <div className="space-y-2">
              <Label htmlFor="folder-dialog-name">
                {mode === "create" ? "Folder name" : "New name"}
              </Label>
              <Input
                id="folder-dialog-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="coding_standards"
                disabled={mode === "rename" && !folderId}
                autoComplete="off"
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="folder-dialog-description">Description (optional)</Label>
              <Textarea
                id="folder-dialog-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="What belongs in here?"
                rows={2}
                disabled={mode === "rename" && !folderId}
              />
              <p className="text-xs text-muted-foreground">
                {FOLDER_DESCRIPTION_HINT}
              </p>
            </div>

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
            <Button type="submit" disabled={!ready || pending}>
              {pending ? "Saving…" : mode === "create" ? "Add folder" : "Rename"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
