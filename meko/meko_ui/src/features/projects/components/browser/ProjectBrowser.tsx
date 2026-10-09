// The folders-and-datapacks browser: selection, search, paging, renaming and
// the add-datapack dialog live here, and SplitPaneLayout is presentation.

import { useEffect, useMemo, useRef, useState } from "react";
import type { Datapack } from "@/lib/api/mekoApi";
import type { ProjectDetail } from "@/lib/api/projectsApi";
import type { DatapackPathMap } from "@/lib/api/projectsApi";
import { PROJECT_SELECTION, type TreeSelection } from "@/features/projects/lib/projectTree";
import { FolderNameDialog } from "@/features/projects/components/FolderNameDialog";
import { AddDatapacksDialog } from "@/features/projects/components/browser/AddDatapacksDialog";
import { SplitPaneLayout } from "@/features/projects/components/browser/layouts/SplitPaneLayout";

/** Everything a layout needs. One object so adding a control does not mean
 *  editing the layout's prop list too. */
export type BrowserProps = {
  detail: ProjectDetail;
  selection: TreeSelection;
  onSelect: (s: TreeSelection) => void;
  isOwner: boolean;
  /** The folder whose contents are showing, if any. */
  openFolderId?: string;
  query: string;
  onQueryChange: (v: string) => void;
  page: number;
  onPageChange: (p: number) => void;
  selectedDatapackId?: string;
  onAddFolder: () => void;
  onRenameFolder: (folderId: string) => void;
  onAddDatapacks: () => void;
  learningsCount: number;
};

export function ProjectBrowser({
  detail,
  isOwner,
  ownDatapacks,
  datapackPaths,
  learningsCount,
}: {
  detail: ProjectDetail;
  isOwner: boolean;
  ownDatapacks: Datapack[];
  datapackPaths: DatapackPathMap;
  learningsCount: number;
}) {
  // Opens on the first folder rather than the project. Landing on the project
  // showed a summary the page header already carries, and left the contents
  // pane closed -- so "Add a datapack", the main thing to do here, was behind
  // a click nobody knew to make. Falls back to the project when there are no
  // folders yet, which is the one case with nothing to open.
  const [selection, setSelection] = useState<TreeSelection>(() =>
    detail.folders.length > 0
      ? { kind: "folder", folderId: detail.folders[0].folder_id }
      : PROJECT_SELECTION,
  );
  const [query, setQuery] = useState("");
  const [page, setPage] = useState(1);
  const [folderDialog, setFolderDialog] = useState<"create" | "rename" | null>(null);
  const [renameFolderId, setRenameFolderId] = useState<string | undefined>();
  const [addOpen, setAddOpen] = useState(false);

  // Which folder's contents are on screen. A datapack selection keeps its
  // folder open, so choosing one does not collapse the list it came from.
  const openFolderId = useMemo(() => {
    if (selection.kind === "folder") return selection.folderId;
    if (selection.kind === "datapack") {
      return detail.datapacks.find((d) => d.datapack_id === selection.datapackId)?.folder_id;
    }
    return undefined;
  }, [selection, detail.datapacks]);

  // A new folder starts at the top of its list with no leftover search.
  useEffect(() => {
    setQuery("");
    setPage(1);
  }, [openFolderId]);

  // The initializer above runs once, so a project that had no folders when it
  // mounted keeps PROJECT_SELECTION after its first folder is created -- the
  // contents pane stays shut and "Add a datapack" is back behind a click.
  // Only on the empty -> non-empty edge, so selecting the project row on a
  // project that already has folders is never overridden.
  const hadFolders = useRef(detail.folders.length > 0);
  useEffect(() => {
    const has = detail.folders.length > 0;
    if (!hadFolders.current && has) {
      setSelection({ kind: "folder", folderId: detail.folders[0].folder_id });
    }
    hadFolders.current = has;
  }, [detail.folders]);

  const props: BrowserProps = {
    detail,
    selection,
    onSelect: setSelection,
    isOwner,
    openFolderId,
    query,
    onQueryChange: (v) => {
      setQuery(v);
      setPage(1);
    },
    page,
    onPageChange: setPage,
    selectedDatapackId: selection.kind === "datapack" ? selection.datapackId : undefined,
    onAddFolder: () => {
      setRenameFolderId(undefined);
      setFolderDialog("create");
    },
    onRenameFolder: (folderId) => {
      setRenameFolderId(folderId);
      setFolderDialog("rename");
    },
    onAddDatapacks: () => setAddOpen(true),
    learningsCount,
  };

  return (
    <>
      <SplitPaneLayout {...props} />

      <FolderNameDialog
        open={folderDialog !== null}
        onOpenChange={(open) => setFolderDialog(open ? folderDialog : null)}
        projectId={detail.project.project_id}
        folders={detail.folders}
        mode={folderDialog ?? "create"}
        initialFolderId={renameFolderId}
      />

      <AddDatapacksDialog
        open={addOpen}
        onOpenChange={setAddOpen}
        folder={detail.folders.find((f) => f.folder_id === openFolderId)}
        nodes={detail.datapacks}
        ownDatapacks={ownDatapacks}
        pathByDatapack={datapackPaths}
      />
    </>
  );
}
