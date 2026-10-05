// The two-level shape of a collection, drawn: a collection holds folders, a
// folder holds datapacks, and folders do not nest.
//
// Shared: the "What is a Collection?" explainer draws an existing collection
// with it, and the New Collection dialog draws the layout you are about to
// create. Both are the same picture of the same idea, so they are the same
// component; the explainer's version used to be private to that file.

import { Database, Folder, FolderOpen } from "lucide-react";
import { cn } from "@/lib/utils";
import { countLabel } from "@/features/collections/collectionsCopy";

/** At most this many folders are drawn; the rest collapse into a count. Keeps
 *  the diagram the same height whether a collection has three folders or
 *  thirty, which matters most in the dialog where it must not jump as the
 *  layout changes. */
const MAX_FOLDERS_DRAWN = 3;

export function FolderTreePreview({
  label,
  name,
  folders,
  footer,
  pendingFolderIndex,
  className,
}: {
  /** Small caps line above the tree, e.g. "Example" or "Your collection". */
  label: string;
  name: string;
  folders: string[];
  /** The bottom line, describing what sits inside the folders. */
  footer: string;
  /** Index of a folder the user is still typing, drawn dashed so it reads as
   *  forming rather than existing. Used by the Custom layout. */
  pendingFolderIndex?: number;
  className?: string;
}) {
  const drawn = folders.slice(0, MAX_FOLDERS_DRAWN);
  const hidden = folders.length - drawn.length;

  return (
    <div
      className={cn(
        "w-full shrink-0 rounded-xl border border-border bg-background/60 p-5",
        className,
      )}
    >
      <p className="text-xs uppercase tracking-wide text-muted-foreground">{label}</p>

      <div className="mt-3 flex items-center gap-2">
        <Folder className="h-4 w-4 shrink-0 text-brand" aria-hidden />
        <span className="truncate font-mono text-sm font-semibold text-foreground" title={name}>
          {name}
        </span>
      </div>

      {/* The rail is the nesting: everything inside it is one level down. */}
      <ul className="mt-2 space-y-1.5 border-l border-border pl-4">
        {drawn.map((folder, i) => (
          <li
            key={`${folder}-${i}`}
            className={cn(
              "flex items-center gap-2",
              i === pendingFolderIndex && "opacity-70",
            )}
          >
            <FolderOpen
              className={cn(
                "h-3.5 w-3.5 shrink-0",
                i === pendingFolderIndex ? "text-brand" : "text-muted-foreground",
              )}
              aria-hidden
            />
            <span
              className={cn(
                "truncate font-mono text-xs text-foreground/90",
                i === pendingFolderIndex &&
                  "rounded border border-dashed border-brand/50 px-1 text-brand",
              )}
              title={folder}
            >
              {folder}
            </span>
          </li>
        ))}
        {hidden > 0 ? (
          <li className="pl-[1.375rem] text-xs text-muted-foreground">
            +{countLabel(hidden, "more folder")}
          </li>
        ) : null}

        <li className="flex items-center gap-2 pt-1">
          <Database className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
          <span className="text-xs text-muted-foreground">{footer}</span>
        </li>
      </ul>
    </div>
  );
}
