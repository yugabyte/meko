// The depth-1 folder names of a project, as monospace chips. Shows the shape
// of the tree at a glance, which is the thing that distinguishes one project
// from another on a list of otherwise identical cards.
//
// Shared: the project card and the unfiled/empty states both render these.

import { cn } from "@/lib/utils";
import { LABEL_NAME_FONT } from "@/features/projects/projectsTypography";

/** Beyond this the chips wrap onto a third line and stop being scannable; the
 *  remainder collapses into a "+N" chip. */
const MAX_VISIBLE_FOLDERS = 4;

export function ProjectFolderChips({
  folders,
  className,
}: {
  folders: string[];
  className?: string;
}) {
  if (folders.length === 0) {
    return (
      <p className={cn("text-xs italic text-muted-foreground/70", className)}>No folders yet</p>
    );
  }

  const visible = folders.slice(0, MAX_VISIBLE_FOLDERS);
  const hidden = folders.length - visible.length;

  return (
    <div className={cn("flex flex-wrap items-center gap-1.5", className)}>
      {visible.map((folder) => (
        <span
          key={folder}
          className={cn("rounded-md bg-muted px-1.5 py-0.5 text-xs text-muted-foreground", LABEL_NAME_FONT)}
        >
          {folder}
        </span>
      ))}
      {hidden > 0 ? (
        <span
          className="text-xs text-muted-foreground/70"
          title={folders.slice(MAX_VISIBLE_FOLDERS).join(", ")}
        >
          +{hidden}
        </span>
      ) : null}
    </div>
  );
}
