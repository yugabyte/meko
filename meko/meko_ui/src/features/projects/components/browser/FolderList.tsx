// The folders in a project, as a selectable list.
//
// The rail of SplitPaneLayout. The pencil on the selected row opens
// FolderNameDialog rather than editing in place, so renaming and creating
// share one validated form.
//
// Datapacks are NOT nested under folders any more. A folder's contents are
// the next pane's job, which is what lets this list stay short enough to see
// at a glance however many datapacks a folder holds.

import { ChevronRight, Folder, FolderTree, Pencil, Plus, Sparkles } from "lucide-react";
import { Button } from "@/components/ui/button";
import { TruncatedText } from "@/components/ui/truncated-text";
import { LABEL_NAME_FONT } from "@/features/projects/projectsTypography";
import { cn } from "@/lib/utils";
import type { ProjectDatapackNode, ProjectFolder } from "@/lib/api/projectsApi";
import { isSameSelection, type TreeSelection } from "@/features/projects/lib/projectTree";
import {
  ADD_FOLDER_LABEL,
  countLabel,
  LEARNINGS_NODE_LABEL,
  RENAME_FOLDER_LABEL,
} from "@/features/projects/projectsCopy";

export function FolderList({
  projectName,
  folders,
  datapacks,
  selection,
  onSelect,
  isOwner,
  onAddFolder,
  onRenameFolder,
  learningsCount,
  className,
}: {
  projectName: string;
  folders: ProjectFolder[];
  datapacks: ProjectDatapackNode[];
  selection: TreeSelection;
  onSelect: (s: TreeSelection) => void;
  isOwner: boolean;
  onAddFolder: () => void;
  onRenameFolder: (folderId: string) => void;
  learningsCount: number;
  className?: string;
}) {
  const countIn = (folderId: string) => datapacks.filter((d) => d.folder_id === folderId).length;

  return (
    <div className={cn("flex min-h-0 flex-col rounded-xl border border-border bg-card", className)}>
      <div className="flex items-center justify-between gap-2 border-b border-border px-4 py-3">
        <h2 className="truncate text-sm font-semibold text-foreground">Folders</h2>
        {/* Top right of the folder list, because this is the thing it adds to. */}
        {isOwner ? (
          <Button
            type="button"
            variant="secondary"
            size="sm"
            className="gap-1.5"
            onClick={onAddFolder}
          >
            <Plus className="h-3.5 w-3.5" aria-hidden />
            {ADD_FOLDER_LABEL}
          </Button>
        ) : null}
      </div>

      <ul className="min-h-0 flex-1 overflow-y-auto p-2">
        <FolderRow
          icon={<FolderTree className="h-4 w-4 shrink-0 text-brand" aria-hidden />}
          label={projectName}
          meta={countLabel(datapacks.length, "datapack")}
          selected={isSameSelection(selection, { kind: "project" })}
          onClick={() => onSelect({ kind: "project" })}
        />

        {folders.map((f) => {
          const selected = isSameSelection(selection, { kind: "folder", folderId: f.folder_id });
          return (
            <FolderRow
              key={f.folder_id}
              indent
              icon={<Folder className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />}
              label={f.folder_name}
              description={f.folder_description}
              meta={countLabel(countIn(f.folder_id), "datapack")}
              selected={selected}
              onClick={() => onSelect({ kind: "folder", folderId: f.folder_id })}
              // Inline, on the row it renames, and only on the selected one so
              // the list is not a column of pencils.
              action={
                isOwner && selected ? (
                  <button
                    type="button"
                    aria-label={`${RENAME_FOLDER_LABEL}: ${f.folder_name}`}
                    title={RENAME_FOLDER_LABEL}
                    className="rounded p-1 text-muted-foreground transition-colors hover:bg-foreground/10 hover:text-foreground"
                    onClick={(e) => {
                      e.stopPropagation();
                      onRenameFolder(f.folder_id);
                    }}
                  >
                    <Pencil className="h-3.5 w-3.5" aria-hidden />
                  </button>
                ) : null
              }
            />
          );
        })}

        <FolderRow
          indent
          icon={<Sparkles className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />}
          label={LEARNINGS_NODE_LABEL}
          meta={countLabel(learningsCount, "entry", "entries")}
          selected={isSameSelection(selection, { kind: "learnings" })}
          onClick={() => onSelect({ kind: "learnings" })}
        />
      </ul>
    </div>
  );
}

function FolderRow({
  icon,
  label,
  description,
  meta,
  selected,
  onClick,
  indent = false,
  action,
}: {
  icon: React.ReactNode;
  label: string;
  description?: string;
  meta: string;
  selected: boolean;
  onClick: () => void;
  indent?: boolean;
  action?: React.ReactNode;
}) {
  return (
    <li>
      <div
        role="button"
        tabIndex={0}
        aria-current={selected ? "true" : undefined}
        onClick={onClick}
        onKeyDown={(e) => {
          if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            onClick();
          }
        }}
        className={cn(
          "group flex w-full cursor-pointer items-center gap-2 rounded-lg px-2 py-2 text-left transition-colors",
          "focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-brand",
          indent && "pl-5",
          selected
            ? // The selected row carries the brand edge that the detail pane
              // picks up, which is what ties the two sides together.
              "border-l-2 border-brand bg-brand/10"
            : "border-l-2 border-transparent hover:bg-foreground/5",
        )}
      >
        {icon}
        <div className="min-w-0 flex-1">
          <TruncatedText
            text={label}
            className={cn("text-sm text-foreground", LABEL_NAME_FONT, selected && "font-medium")}
          />
          {description ? (
            <TruncatedText text={description} className="text-xs text-muted-foreground" />
          ) : null}
        </div>
        {action}
        <span className="shrink-0 text-xs tabular-nums text-muted-foreground">{meta}</span>
        <ChevronRight
          className={cn(
            "h-3.5 w-3.5 shrink-0 transition-colors",
            selected ? "text-brand" : "text-transparent group-hover:text-muted-foreground",
          )}
          aria-hidden
        />
      </div>
    </li>
  );
}
