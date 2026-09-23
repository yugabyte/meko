// Create a collection: name it and pick one of the fixed folder layouts the API
// offers. The layout is stamped at creation — folders can be edited afterwards
// and the theme is never consulted again — so the picker previews the folders
// it will create rather than describing an ongoing link.

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Spinner } from "@/components/ui/spinner";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Collection } from "@/lib/api/collectionsApi";
import { useCreateCollection, useCollectionThemes } from "@/features/collections/hooks/useCollections";
import {
  NEW_COLLECTION_LABEL,
  COLLECTION_TOASTS,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/collections/collectionsCopy";
import { CollectionThemeOption } from "@/features/collections/components/CollectionThemeOption";

export function NewCollectionDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (collection: Collection) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [themeKey, setThemeKey] = useState("");
  // The Custom layout ships no folders, so the user names the first one.
  const [customFolder, setCustomFolder] = useState("");
  const [error, setError] = useState<string | null>(null);

  // Only fetched once the dialog opens; the registry is a server constant so
  // it never refetches afterwards.
  const { data: themes = [], isLoading: themesLoading } = useCollectionThemes({ enabled: open });
  const createMut = useCreateCollection();

  useEffect(() => {
    if (!open) return;
    setName("");
    setDescription("");
    setError(null);
    setThemeKey("");
    setCustomFolder("");
  }, [open]);

  // Preselect the first layout once the registry arrives, so the form is
  // submittable without the user having to notice the picker.
  useEffect(() => {
    if (open && themeKey === "" && themes.length > 0) {
      setThemeKey(themes[0].key);
    }
  }, [open, themeKey, themes]);

  const selectedTheme = themes.find((t) => t.key === themeKey);
  // A layout with no folders of its own is the signal to ask for one, rather
  // than hardcoding which theme key that happens to be.
  const needsFolderName = Boolean(selectedTheme && selectedTheme.folders.length === 0);
  const canSubmit =
    name.trim() !== "" && themeKey !== "" && (!needsFolderName || customFolder.trim() !== "");

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!canSubmit || createMut.isPending) return;
    setError(null);
    try {
      const collection = await createMut.mutateAsync({
        collection_name: trimmed,
        description: description.trim(),
        theme_key: themeKey,
        ...(needsFolderName ? { folders: [customFolder.trim()] } : {}),
      });
      onOpenChange(false);
      onCreated(collection);
      toast({ title: COLLECTION_TOASTS.collectionCreated.ok, description: trimmed });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: COLLECTION_TOASTS.collectionCreated.fail,
        description: message,
      });
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-lg bg-card border-border text-foreground">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void handleCreate();
          }}
        >
          <DialogHeader>
            <DialogTitle>{NEW_COLLECTION_LABEL}</DialogTitle>
            <DialogDescription>
              Pick a layout to start with. Its folders are created now, and you can add, rename
              or remove them later.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            <div className="space-y-2">
              <Label htmlFor="collection-name">Name</Label>
              <Input
                id="collection-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder="database_dev"
                autoFocus
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="collection-description">Description (optional)</Label>
              <Textarea
                id="collection-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="What outcome do these datapacks serve?"
                rows={2}
              />
            </div>

            <fieldset className="space-y-2">
              <legend className="text-sm font-medium text-foreground">Layout</legend>
              {themesLoading ? (
                <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
                  <Spinner className="h-4 w-4" />
                  Loading layouts…
                </div>
              ) : (
                <div className="space-y-2">
                  {themes.map((theme) => (
                    <CollectionThemeOption
                      key={theme.key}
                      theme={theme}
                      selected={theme.key === themeKey}
                      onSelect={setThemeKey}
                    />
                  ))}
                </div>
              )}
            </fieldset>

            {needsFolderName ? (
              <div className="space-y-2">
                <Label htmlFor="collection-new-folder">New Folder</Label>
                <Input
                  id="collection-new-folder"
                  value={customFolder}
                  onChange={(e) => setCustomFolder(e.target.value)}
                  placeholder="standards"
                />
                <p className="text-xs text-muted-foreground">
                  Datapacks live inside folders, so a collection needs at least one. You can add more
                  later.
                </p>
              </div>
            ) : null}

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
            <Button type="submit" disabled={!canSubmit || createMut.isPending}>
              {createMut.isPending ? "Creating…" : "Create collection"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
