// The collection's shape as a clickable tree: the collection, its folders, the
// datapacks inside each folder, and one learnings node for the collection.
//
// Selecting a row drives the panel beside it. Every number here comes from
// the datapack rows the collection response already carried, so expanding a
// folder issues no request.

import { useState } from "react";
import { ChevronDown, ChevronRight, Database, Folder, FolderOpen, Sparkles } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import type { CollectionDatapackNode, CollectionFolder } from "@/lib/api/collectionsApi";
import {
  isSameSelection,
  nodesInFolder,
  sumNodes,
  type TreeSelection,
} from "@/features/collections/lib/collectionTree";
import { countLabel, LEARNINGS_NODE_LABEL, TREE_HINT } from "@/features/collections/collectionsCopy";

/** One row. Indent is a left margin rather than nesting, so every row's hit
 *  area spans the full width of the card whatever its depth. */
function TreeRow({
  icon: Icon,
  label,
  meta,
  depth,
  selected,
  mono = true,
  onSelect,
  expander,
}: {
  icon: typeof Folder;
  label: string;
  meta: string;
  depth: 0 | 1 | 2;
  selected: boolean;
  /** Folder and datapack names are identifiers; the learnings node is a word. */
  mono?: boolean;
  onSelect: () => void;
  /** Rendered before the icon when the row can be opened. */
  expander?: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onSelect}
      aria-current={selected}
      className={cn(
        "flex w-full items-center gap-2 rounded-md py-2 pr-3 text-left transition-colors",
        depth === 0 && "pl-3",
        depth === 1 && "pl-6",
        depth === 2 && "pl-12",
        selected ? "bg-brand/10 ring-1 ring-brand/30" : "hover:bg-muted/50",
      )}
    >
      {expander ?? <span className="w-4 shrink-0" aria-hidden />}
      <Icon
        className={cn("h-4 w-4 shrink-0", selected ? "text-brand" : "text-muted-foreground")}
        aria-hidden
      />
      <span
        className={cn(
          "min-w-0 flex-1 truncate text-sm text-foreground",
          mono && "font-mono",
          depth === 0 && "font-semibold",
        )}
        title={label}
      >
        {label}
      </span>
      <span className="shrink-0 text-xs text-muted-foreground">{meta}</span>
    </button>
  );
}

export function CollectionTree({
  collectionName,
  folders,
  datapacks,
  selection,
  onSelect,
}: {
  collectionName: string;
  folders: CollectionFolder[];
  datapacks: CollectionDatapackNode[];
  selection: TreeSelection;
  onSelect: (selection: TreeSelection) => void;
}) {
  const [openFolders, setOpenFolders] = useState<Record<string, boolean>>({});
  const totals = sumNodes(datapacks);

  const toggle = (folderId: string) =>
    setOpenFolders((open) => ({ ...open, [folderId]: !open[folderId] }));

  return (
    <Card className="border-border bg-card p-2">
      <div className="flex items-center justify-between px-3 pb-1 pt-2">
        <h2 className="text-sm font-semibold text-foreground">Folders &amp; datapacks</h2>
        <span className="text-xs text-muted-foreground">{TREE_HINT}</span>
      </div>

      <ul className="space-y-0.5">
        <li>
          <TreeRow
            icon={Folder}
            label={collectionName}
            meta={countLabel(totals.datapacks, "datapack")}
            depth={0}
            selected={isSameSelection(selection, { kind: "collection" })}
            onSelect={() => onSelect({ kind: "collection" })}
          />
        </li>

        {folders.map((folder) => {
          const inside = nodesInFolder(datapacks, folder.folder_id);
          const isOpen = openFolders[folder.folder_id] ?? false;
          return (
            <li key={folder.folder_id}>
              <TreeRow
                icon={isOpen ? FolderOpen : Folder}
                label={folder.folder_name}
                meta={countLabel(inside.length, "datapack")}
                depth={1}
                selected={isSameSelection(selection, {
                  kind: "folder",
                  folderId: folder.folder_id,
                })}
                onSelect={() => {
                  onSelect({ kind: "folder", folderId: folder.folder_id });
                  if (inside.length > 0) toggle(folder.folder_id);
                }}
                expander={
                  // A span, not a nested button: the whole row already toggles.
                  <span
                    className={cn("shrink-0 text-muted-foreground", inside.length === 0 && "opacity-0")}
                    aria-hidden
                  >
                    {isOpen ? (
                      <ChevronDown className="h-4 w-4" />
                    ) : (
                      <ChevronRight className="h-4 w-4" />
                    )}
                  </span>
                }
              />

              {isOpen ? (
                <ul className="space-y-0.5">
                  {inside.map((node) => (
                    <li key={node.datapack_id}>
                      <TreeRow
                        icon={Database}
                        label={node.datapack_name}
                        meta={`${node.memory_count} memories · ${node.promoted_count} promoted`}
                        depth={2}
                        selected={isSameSelection(selection, {
                          kind: "datapack",
                          datapackId: node.datapack_id,
                        })}
                        onSelect={() => onSelect({ kind: "datapack", datapackId: node.datapack_id })}
                      />
                    </li>
                  ))}
                </ul>
              ) : null}
            </li>
          );
        })}

        <li>
          <TreeRow
            icon={Sparkles}
            label={LEARNINGS_NODE_LABEL}
            meta={countLabel(totals.awaitingReview, "entry", "entries")}
            depth={1}
            mono={false}
            selected={isSameSelection(selection, { kind: "learnings" })}
            onSelect={() => onSelect({ kind: "learnings" })}
          />
        </li>
      </ul>
    </Card>
  );
}
