// Layout A — split pane.
//
// Folders on the left, contents in the middle once a folder is open, detail
// on the right. The folder rail NARROWS when a folder opens (it has done its
// job, and the contents need the room) and widens back when the selection
// returns to the collection.
//
// Closest to what was there before: the same two panes, with the folder's
// datapacks promoted out of the tree into a pane of their own.

import { cn } from "@/lib/utils";
import type { BrowserProps } from "@/features/collections/components/browser/CollectionBrowser";
import { FolderList } from "@/features/collections/components/browser/FolderList";
import { FolderContentsPane } from "@/features/collections/components/browser/FolderContentsPane";
import { SelectionDetailPane } from "@/features/collections/components/browser/SelectionDetailPane";

export function SplitPaneLayout(p: BrowserProps) {
  const folder = p.detail.folders.find((f) => f.folder_id === p.openFolderId);

  return (
    // Fractional rather than 12 columns: the folder rail needs a little more
    // than 3/12 to stop truncating names, and the contents pane is where the
    // work happens, so it takes the space the detail pane gives up.
    <div
      className={cn(
        "grid h-[32rem] gap-4",
        folder ? "lg:grid-cols-[3.5fr_5.5fr_4fr]" : "lg:grid-cols-[5fr_7fr]",
      )}
    >
      <FolderList
        collectionName={p.detail.collection.collection_name}
        folders={p.detail.folders}
        datapacks={p.detail.datapacks}
        selection={p.selection}
        onSelect={p.onSelect}
        isOwner={p.isOwner}
        onAddFolder={p.onAddFolder}
        onRenameFolder={p.onRenameFolder}
        learningsCount={p.learningsCount}
        className="transition-all duration-300"
      />

      {folder ? (
        <FolderContentsPane
          folder={folder}
          datapacks={p.detail.datapacks}
          query={p.query}
          onQueryChange={p.onQueryChange}
          page={p.page}
          onPageChange={p.onPageChange}
          selectedDatapackId={p.selectedDatapackId}
          onSelectDatapack={(id) => p.onSelect({ kind: "datapack", datapackId: id })}
          isOwner={p.isOwner}
          onAddDatapacks={p.onAddDatapacks}
          className="animate-in fade-in slide-in-from-left-2 duration-300"
        />
      ) : null}

      <SelectionDetailPane
        detail={p.detail}
        selection={p.selection}
        className="transition-all duration-300"
      />
    </div>
  );
}
