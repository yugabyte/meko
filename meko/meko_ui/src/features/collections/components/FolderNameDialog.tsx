// Add a folder to this collection, or rename one of its folders.
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
import type { CollectionFolder } from "@/lib/api/collectionsApi";
import { useCreateFolder, useRenameFolder } from "@/features/collections/hooks/useCollections";
import {
  ADD_FOLDER_LABEL,
  COLLECTION_TOASTS,
  RENAME_FOLDER_LABEL,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/collections/collectionsCopy";

export function FolderNameDialog({
  open,
  onOpenChange,
  collectionId,
  folders,
  mode,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  collectionId: string;
  /** Only read in rename mode, to choose which folder to act on. */
  folders: CollectionFolder[];
  mode: "create" | "rename";
}) {
  const [folderId, setFolderId] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const createMut = useCreateFolder(collectionId);
  const renameMut = useRenameFolder(collectionId);
  const pending = createMut.isPending || renameMut.isPending;

  useEffect(() => {
    if (!open) return;
    setFolderId("");
    setName("");
    setError(null);
  }, [open, mode]);

  // Renaming starts from the current name, which is what the user is editing.
  useEffect(() => {
    if (mode !== "rename" || !folderId) return;
    setName(folders.find((f) => f.folder_id === folderId)?.folder_name ?? "");
  }, [mode, folderId, folders]);

  const trimmed = name.trim();
  const ready = trimmed !== "" && (mode === "create" || folderId !== "");

  const handleSubmit = async () => {
    if (!ready || pending) return;
    // The dialog closes on success, so the toast is the only thing left
    // saying the folder was created; the inline error covers the failure
    // case for a user still looking at the form.
    const copy = mode === "create" ? COLLECTION_TOASTS.folderAdded : COLLECTION_TOASTS.folderRenamed;
    setError(null);
    try {
      if (mode === "create") {
        await createMut.mutateAsync({ folder_name: trimmed });
      } else {
        await renameMut.mutateAsync({ folderId, folderName: trimmed });
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
                : "Datapacks inside keep their place; only the name changes."}
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
