// Create a project: name it and pick one of the fixed folder layouts the API
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Project } from "@/lib/api/projectsApi";
import { useCreateProject, useProjectThemes } from "@/features/projects/hooks/useProjects";
import {
  NEW_PROJECT_LABEL,
  NEW_FOLDER_LABEL,
  layoutExampleName,
  PROJECT_TOASTS,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/projects/projectsCopy";
import { FolderTreePreview } from "@/features/projects/components/FolderTreePreview";

export function NewProjectDialog({
  open,
  onOpenChange,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCreated: (project: Project) => void;
}) {
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [themeKey, setThemeKey] = useState("");
  // The Custom layout ships no folders, so the user names the first one.
  const [customFolder, setCustomFolder] = useState("");
  const [error, setError] = useState<string | null>(null);

  // Only fetched once the dialog opens; the registry is a server constant so
  // it never refetches afterwards.
  const { data: themes = [], isLoading: themesLoading } = useProjectThemes({ enabled: open });
  const createMut = useCreateProject();

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
  // What the preview draws. The Custom layout has no folders of its own, so
  // it shows the one the user is typing, live, rather than an empty tree.
  const exampleName = layoutExampleName(themeKey);
  const previewName = name.trim() || exampleName;
  const previewFolders = needsFolderName
    ? [customFolder.trim() || "new_folder"]
    : (selectedTheme?.folders ?? []);
  const confirmQuestion = needsFolderName
    ? `Create ${previewName} with a folder of your own?`
    : `Create ${previewName} using the ${selectedTheme?.label ?? ""} layout?`;

  const canSubmit =
    name.trim() !== "" && themeKey !== "" && (!needsFolderName || customFolder.trim() !== "");

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!canSubmit || createMut.isPending) return;
    setError(null);
    try {
      const project = await createMut.mutateAsync({
        project_name: trimmed,
        description: description.trim(),
        theme_key: themeKey,
        ...(needsFolderName ? { folders: [customFolder.trim()] } : {}),
      });
      onOpenChange(false);
      onCreated(project);
      toast({ title: PROJECT_TOASTS.projectCreated.ok, description: trimmed });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: PROJECT_TOASTS.projectCreated.fail,
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
            <DialogTitle>{NEW_PROJECT_LABEL}</DialogTitle>
            <DialogDescription>
              Pick a layout to start with. Its folders are created now, and you can add, rename
              or remove them later.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-4 py-4">
            <div className="space-y-2">
              <Label htmlFor="project-name">Name</Label>
              <Input
                id="project-name"
                value={name}
                onChange={(e) => setName(e.target.value)}
                placeholder={exampleName}
                autoFocus
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="project-description">Description (optional)</Label>
              <Textarea
                id="project-description"
                value={description}
                onChange={(e) => setDescription(e.target.value)}
                placeholder="What outcome do these datapacks serve?"
                rows={2}
              />
            </div>

            <div className="space-y-2">
              <Label htmlFor="project-layout">Layout</Label>
              {themesLoading ? (
                <div className="flex items-center gap-2 py-4 text-sm text-muted-foreground">
                  <Spinner className="h-4 w-4" />
                  Loading layouts…
                </div>
              ) : (
                <Select value={themeKey} onValueChange={setThemeKey}>
                  <SelectTrigger id="project-layout">
                    <SelectValue placeholder="Choose a layout" />
                  </SelectTrigger>
                  <SelectContent>
                    {themes.map((theme) => (
                      <SelectItem key={theme.key} value={theme.key}>
                        {theme.label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              )}
              {selectedTheme ? (
                <p className="text-xs leading-relaxed text-muted-foreground">
                  {selectedTheme.description}
                </p>
              ) : null}
            </div>

            {needsFolderName ? (
              <div className="space-y-2">
                <Label htmlFor="project-new-folder">{NEW_FOLDER_LABEL}</Label>
                <Input
                  id="project-new-folder"
                  value={customFolder}
                  onChange={(e) => setCustomFolder(e.target.value)}
                  placeholder="standards"
                />
                <p className="text-xs text-muted-foreground">
                  Watch it appear in the shape below. You can add more folders later.
                </p>
              </div>
            ) : null}

            {/* The shape you are about to create, drawn with your own name in
                it. Keyed on the layout so switching animates rather than
                swapping silently, which is the whole reason the picker lost
                its three description blocks: the drawing says it faster. */}
            {selectedTheme ? (
              <div
                key={themeKey}
                className="animate-in fade-in slide-in-from-top-1 duration-200"
              >
                <FolderTreePreview
                  label="You will get"
                  name={previewName}
                  folders={previewFolders}
                  footer="datapacks you file into each folder"
                  pendingFolderIndex={needsFolderName ? 0 : undefined}
                />
                <p className="mt-3 text-sm text-foreground">
                  {confirmQuestion}
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
              {createMut.isPending ? "Creating…" : "Create project"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
