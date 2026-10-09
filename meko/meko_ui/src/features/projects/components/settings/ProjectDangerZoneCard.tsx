// The two deletes that destroy content rather than structure, and the
// type-the-name guard in front of each.
//
// Both are owner-only, which the Settings tab enforces by not rendering for
// anyone else. Only datapacks the caller OWNS are offered: a shared datapack
// filed here belongs to someone else's account.

import { useMemo, useState } from "react";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Combobox, type ComboboxOption } from "@/components/ui/combobox";
import { ConfirmByTypingDialog } from "@/components/ui/confirm-by-typing-dialog";
import { Label } from "@/components/ui/label";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Datapack } from "@/lib/api/mekoApi";
import type { Project, ProjectDatapackNode } from "@/lib/api/projectsApi";
import { useDeleteDatapackInTree, useDeleteProject } from "@/features/projects/hooks/useProjects";
import {
  DANGER_ZONE_BODY,
  DANGER_ZONE_TITLE,
  DELETE_DATAPACK_LABEL,
  DELETE_PROJECT_BODY,
  DELETE_PROJECT_LABEL,
  deleteProjectDatapackWarning,
  PROJECT_TOASTS,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/projects/projectsCopy";

export function ProjectDangerZoneCard({
  project,
  datapacks,
  ownDatapacks,
  onProjectDeleted,
}: {
  project: Project;
  /** Every datapack in this project, for the delete count and the picker. */
  datapacks: ProjectDatapackNode[];
  ownDatapacks: Datapack[];
  onProjectDeleted: () => void;
}) {
  const [deleteProjectOpen, setDeleteProjectOpen] = useState(false);
  const [deleteDatapackOpen, setDeleteDatapackOpen] = useState(false);
  const [doomedId, setDoomedId] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const deleteProjectMut = useDeleteProject();
  const deleteDatapackMut = useDeleteDatapackInTree();

  // Memoized together: `deletable` is derived, so memoizing only the options
  // off it would rebuild them every render anyway.
  const { deletable, deletableOptions } = useMemo(() => {
    const ownedIds = new Set(ownDatapacks.map((d) => d.datapack_id));
    const owned = datapacks.filter((d) => ownedIds.has(d.datapack_id));
    return {
      deletable: owned,
      // Keyed by id, not name: two datapacks may share a name, and cmdk
      // collapses items that share a value into one keyboard target.
      deletableOptions: owned.map<ComboboxOption>((d) => ({
        value: d.datapack_id,
        label: d.datapack_name,
      })),
    };
  }, [datapacks, ownDatapacks]);

  const doomed = deletable.find((d) => d.datapack_id === doomedId);

  const handleDeleteProject = async () => {
    setPending(true);
    setError(null);
    try {
      await deleteProjectMut.mutateAsync(project.project_id);
      setDeleteProjectOpen(false);
      onProjectDeleted();
      // Navigates away on success, so the toast is the only confirmation
      // that survives the page change.
      toast({ title: PROJECT_TOASTS.projectDeleted.ok, description: project.project_name });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: PROJECT_TOASTS.projectDeleted.fail,
        description: message,
      });
    } finally {
      setPending(false);
    }
  };

  const handleDeleteDatapack = async () => {
    if (!doomed) return;
    setPending(true);
    setError(null);
    try {
      await deleteDatapackMut.mutateAsync(doomed.datapack_id);
      setDeleteDatapackOpen(false);
      setDoomedId("");
      toast({ title: PROJECT_TOASTS.datapackDeleted.ok, description: doomed.datapack_name });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: PROJECT_TOASTS.datapackDeleted.fail,
        description: message,
      });
    } finally {
      setPending(false);
    }
  };

  /** Shown inside both dialogs, so a failure lands where the user is looking. */
  const errorLine = error ? (
    <span role="alert" className="mt-2 block text-destructive">
      {error}
    </span>
  ) : null;

  return (
    <Card className="border-destructive/40 bg-card p-6">
      <h2 className="text-base font-semibold text-destructive">{DANGER_ZONE_TITLE}</h2>
      <p className="mt-2 text-sm text-muted-foreground">{DANGER_ZONE_BODY}</p>
      <div className="mt-4 flex flex-wrap gap-2">
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={deletable.length === 0}
          onClick={() => {
            setDoomedId("");
            setError(null);
            setDeleteDatapackOpen(true);
          }}
        >
          {DELETE_DATAPACK_LABEL}
        </Button>
        <Button
          type="button"
          variant="destructive"
          size="sm"
          onClick={() => {
            setError(null);
            setDeleteProjectOpen(true);
          }}
        >
          {DELETE_PROJECT_LABEL}
        </Button>
      </div>

      <ConfirmByTypingDialog
        open={deleteProjectOpen}
        onOpenChange={setDeleteProjectOpen}
        title={`Delete ${project.project_name}?`}
        description={
          <>
            {DELETE_PROJECT_BODY}{" "}
            <span className="font-medium text-foreground">
              {deleteProjectDatapackWarning(datapacks.length)}
            </span>{" "}
            Type the project name to confirm.
            {errorLine}
          </>
        }
        confirmPhrase={project.project_name}
        confirmLabel="Delete project"
        pending={pending}
        onConfirm={handleDeleteProject}
      />

      <ConfirmByTypingDialog
        open={deleteDatapackOpen}
        onOpenChange={setDeleteDatapackOpen}
        title="Delete a datapack?"
        description={
          <>
            The datapack and everything in it are removed for good: its memories, its knowledge and
            its history. Pick one, then type its name to confirm.
            {errorLine}
          </>
        }
        confirmPhrase={doomed?.datapack_name ?? ""}
        confirmLabel="Delete datapack"
        pending={pending}
        onConfirm={handleDeleteDatapack}
      >
        <Label htmlFor="danger-datapack">Datapack</Label>
        {/* Searchable rather than a plain Select: a project can hold hundreds
            of datapacks, and scrolling an unfiltered list to find the one you
            mean is a poor last step before an irreversible delete. Combobox
            caps the list height and scrolls it.

            modal because this sits inside one: the popover portals outside
            DialogContent, and the Dialog's scroll lock would otherwise cancel
            every wheel event over the list. */}
        <Combobox
          id="danger-datapack"
          modal
          options={deletableOptions}
          value={doomedId}
          onChange={setDoomedId}
          placeholder="Select a datapack"
          searchPlaceholder="Search datapacks…"
          emptyText="No matching datapacks."
          className="w-full"
        />
      </ConfirmByTypingDialog>
    </Card>
  );
}
