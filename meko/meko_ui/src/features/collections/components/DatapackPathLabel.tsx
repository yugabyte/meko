// Where a datapack sits in the caller's tree, shown on a datapack tile. When
// it is unfiled this is the "File into a collection" affordance instead of a
// label — the empty case is the one that needs an action, not a blank.

import { FolderInput, FolderTree } from "lucide-react";
import { cn } from "@/lib/utils";
import type { DatapackPath } from "@/lib/api/collectionsApi";
import { FILE_INTO_COLLECTION_LABEL, UNFILED_LABEL } from "@/features/collections/collectionsCopy";

export function DatapackPathLabel({
  path,
  onFile,
  className,
}: {
  /** Undefined means the datapack is not filed in this user's tree. */
  path?: DatapackPath;
  /** Omitted when the caller cannot file (e.g. no collections exist yet). */
  onFile?: () => void;
  className?: string;
}) {
  if (path) {
    return (
      <p
        className={cn("flex min-w-0 items-center gap-1 text-xs text-muted-foreground", className)}
        title={path.path}
      >
        <FolderTree className="h-3 w-3 shrink-0" aria-hidden />
        <span className="truncate font-mono">{path.path}</span>
      </p>
    );
  }

  if (!onFile) {
    return (
      <p className={cn("text-xs italic text-muted-foreground/70", className)}>{UNFILED_LABEL}</p>
    );
  }

  return (
    <button
      type="button"
      // Stop the click reaching the tile's own open-on-click handler.
      onClick={(e) => {
        e.stopPropagation();
        onFile();
      }}
      className={cn(
        "flex min-w-0 items-center gap-1 rounded text-xs text-muted-foreground underline-offset-2 transition-colors",
        "hover:text-foreground hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-brand",
        className,
      )}
    >
      <FolderInput className="h-3 w-3 shrink-0" aria-hidden />
      <span className="truncate">{FILE_INTO_COLLECTION_LABEL}</span>
    </button>
  );
}
