// What is inside the selected folder: its datapacks, searchable, five at a
// time, with the button that puts more in.
//
// This is the pane that used to be nested rows under the folder in the tree.
// Moving it out is what lets a folder hold thirty datapacks without the tree
// becoming unusable, and it is where the search and paging can live.

import { Database, FolderInput, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { TruncatedText } from "@/components/ui/truncated-text";
import { cn } from "@/lib/utils";
import { useNowMs, formatRelativeTime } from "@/lib/relativeTime";
import type { CollectionDatapackNode, CollectionFolder } from "@/lib/api/collectionsApi";
import { folderContents } from "@/features/collections/lib/folderContents";
import {
  countLabel,
  FOLDER_EMPTY_LABEL,
  ADD_A_DATAPACK_LABEL,
} from "@/features/collections/collectionsCopy";
import {
  DATAPACK_NAME_FONT,
  LABEL_NAME_FONT,
} from "@/features/collections/collectionsTypography";

export function FolderContentsPane({
  folder,
  datapacks,
  query,
  onQueryChange,
  page,
  onPageChange,
  selectedDatapackId,
  onSelectDatapack,
  isOwner,
  onAddDatapacks,
  className,
}: {
  folder: CollectionFolder;
  datapacks: CollectionDatapackNode[];
  query: string;
  onQueryChange: (v: string) => void;
  page: number;
  onPageChange: (p: number) => void;
  selectedDatapackId?: string;
  onSelectDatapack: (id: string) => void;
  isOwner: boolean;
  onAddDatapacks: () => void;
  className?: string;
}) {
  const nowMs = useNowMs();
  const { rows, matchCount, pageCount, page: current } = folderContents(
    datapacks,
    folder.folder_id,
    query,
    page,
  );
  // Search only earns its space once there is more than a screenful.
  const showSearch = datapacks.filter((d) => d.folder_id === folder.folder_id).length > 5;

  return (
    <div className={cn("flex min-h-0 flex-col rounded-xl border border-border bg-card", className)}>
      <div className="space-y-3 border-b border-border px-4 py-3">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <TruncatedText
              text={folder.folder_name}
              side="bottom"
              className={cn("text-sm font-semibold text-foreground", LABEL_NAME_FONT)}
            />
            <p className="mt-0.5 text-xs text-muted-foreground">
              {folder.folder_description || countLabel(matchCount, "datapack")}
            </p>
          </div>
          {isOwner ? (
            <Button
              type="button"
              variant="secondary"
              size="sm"
              className="shrink-0 gap-1.5"
              onClick={onAddDatapacks}
            >
              <FolderInput className="h-3.5 w-3.5" aria-hidden />
              {ADD_A_DATAPACK_LABEL}
            </Button>
          ) : null}
        </div>

        {showSearch ? (
          <div className="relative">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-3.5 w-3.5 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              value={query}
              onChange={(e) => onQueryChange(e.target.value)}
              placeholder={`Search in ${folder.folder_name}...`}
              aria-label={`Search datapacks in ${folder.folder_name}`}
              className="h-8 w-full border-border bg-input pl-8 text-sm"
            />
          </div>
        ) : null}
      </div>

      <ul className="min-h-0 flex-1 overflow-y-auto p-2">
        {rows.length === 0 ? (
          <li className="px-2 py-6 text-center text-sm text-muted-foreground">
            {query.trim() ? "No datapacks match your search." : FOLDER_EMPTY_LABEL}
          </li>
        ) : (
          rows.map((d) => {
            const selected = d.datapack_id === selectedDatapackId;
            return (
              <li key={d.datapack_id}>
                <div
                  role="button"
                  tabIndex={0}
                  aria-current={selected ? "true" : undefined}
                  onClick={() => onSelectDatapack(d.datapack_id)}
                  onKeyDown={(e) => {
                    if (e.key === "Enter" || e.key === " ") {
                      e.preventDefault();
                      onSelectDatapack(d.datapack_id);
                    }
                  }}
                  className={cn(
                    "flex w-full cursor-pointer items-center gap-2 rounded-lg px-2 py-2 text-left transition-colors",
                    "focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-brand",
                    selected
                      ? "border-l-2 border-brand bg-brand/10"
                      : "border-l-2 border-transparent hover:bg-foreground/5",
                  )}
                >
                  <Database className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
                  <div className="min-w-0 flex-1">
                    <TruncatedText
                      text={d.datapack_name}
                      className={cn("text-sm text-foreground", DATAPACK_NAME_FONT, selected && "font-medium")}
                    />
                    <p className="truncate text-xs text-muted-foreground">
                      {countLabel(d.memory_count, "memory", "memories")} ·{" "}
                      {formatRelativeTime(d.updated_at, nowMs)}
                    </p>
                  </div>
                </div>
              </li>
            );
          })
        )}
      </ul>

      {pageCount > 1 ? (
        <div className="flex items-center justify-between gap-2 border-t border-border px-4 py-2">
          <span className="text-xs tabular-nums text-muted-foreground">
            Page {current} of {pageCount}
          </span>
          <div className="flex gap-1">
            <Button
              type="button"
              variant="secondary"
              size="sm"
              disabled={current <= 1}
              onClick={() => onPageChange(current - 1)}
            >
              Previous
            </Button>
            <Button
              type="button"
              variant="secondary"
              size="sm"
              disabled={current >= pageCount}
              onClick={() => onPageChange(current + 1)}
            >
              Next
            </Button>
          </div>
        </div>
      ) : null}
    </div>
  );
}
