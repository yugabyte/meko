// The folders-and-datapacks browser: selection, search, paging, renaming and
// the add-datapack dialog live here, and SplitPaneLayout is presentation.

import { useEffect, useMemo, useState } from "react";
import type { Datapack } from "@/lib/api/mekoApi";
import type { CollectionDetail } from "@/lib/api/collectionsApi";
import type { DatapackPathMap } from "@/lib/api/collectionsApi";
import {
  COLLECTION_SELECTION,
  type TreeSelection,
} from "@/features/collections/lib/collectionTree";
import { FolderNameDialog } from "@/features/collections/components/FolderNameDialog";
import { AddDatapacksDialog } from "@/features/collections/components/browser/AddDatapacksDialog";
import { SplitPaneLayout } from "@/features/collections/components/browser/layouts/SplitPaneLayout";

/** Everything a layout needs. One object so adding a control does not mean
 *  editing the layout's prop list too. */
export type BrowserProps = {
  detail: CollectionDetail;
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

export function CollectionBrowser({
  detail,
  isOwner,
  ownDatapacks,
  datapackPaths,
  learningsCount,
}: {
  detail: CollectionDetail;
  isOwner: boolean;
  ownDatapacks: Datapack[];
  datapackPaths: DatapackPathMap;
  learningsCount: number;
}) {
  const [selection, setSelection] = useState<TreeSelection>(COLLECTION_SELECTION);
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
        collectionId={detail.collection.collection_id}
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
