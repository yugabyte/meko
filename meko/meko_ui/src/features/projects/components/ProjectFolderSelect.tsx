// Pick a destination in the project tree: a project, then a folder inside it.
// Two selects rather than one flat list of every folder the user owns, because
// a folder name is only unique within its project.
//
// Shared: used by the file/move dialog and by the create-datapack
// wizard, which need the same choice at different moments.

import { useEffect, useState } from "react";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useProject, useProjects } from "@/features/projects/hooks/useProjects";

export function ProjectFolderSelect({
  folderId,
  onFolderChange,
  enabled = true,
  idPrefix,
  defaultProjectId = "",
}: {
  /** Selected folder, or "" when nothing is chosen yet. */
  folderId: string;
  onFolderChange: (folderId: string) => void;
  /** Gates both queries, so a disabled picker fetches nothing. */
  enabled?: boolean;
  /** Distinguishes the two label/control pairs when more than one picker can
   *  exist on a page. */
  idPrefix: string;
  /** Preselects a project, for a caller opening this from inside one. Still
   *  changeable: moving a datapack to a different project is allowed. */
  defaultProjectId?: string;
}) {
  const [projectId, setProjectId] = useState(defaultProjectId);
  // Owned only: filing into someone else's project is not a thing you can do.
  const { data: projectLists } = useProjects({ enabled });
  const projects = projectLists?.owned ?? [];
  // Only the chosen project's tree is read — never every tree the user owns.
  const { data: detail, isLoading: foldersLoading } = useProject(
    enabled && projectId ? projectId : undefined,
  );
  const folders = detail?.folders ?? [];

  // Changing project invalidates any folder chosen under the previous one.
  useEffect(() => {
    onFolderChange("");
    // onFolderChange is the parent's setter; depending on it would re-fire
    // this whenever the parent re-renders with a new closure.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId]);

  return (
    <>
      <div className="space-y-2">
        <Label htmlFor={`${idPrefix}-project`}>Project</Label>
        <Select value={projectId} onValueChange={setProjectId} disabled={!enabled}>
          <SelectTrigger id={`${idPrefix}-project`}>
            <SelectValue placeholder="Select a project" />
          </SelectTrigger>
          <SelectContent>
            {projects.map((p) => (
              <SelectItem key={p.project_id} value={p.project_id}>
                {p.project_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="space-y-2">
        <Label htmlFor={`${idPrefix}-folder`}>Folder</Label>
        <Select value={folderId} onValueChange={onFolderChange} disabled={!enabled || !projectId}>
          <SelectTrigger id={`${idPrefix}-folder`}>
            <SelectValue
              placeholder={
                !projectId
                  ? "Pick a project first"
                  : foldersLoading
                    ? "Loading folders…"
                    : "Select a folder"
              }
            />
          </SelectTrigger>
          <SelectContent>
            {folders.map((folder) => (
              <SelectItem key={folder.folder_id} value={folder.folder_id}>
                {folder.folder_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </>
  );
}
