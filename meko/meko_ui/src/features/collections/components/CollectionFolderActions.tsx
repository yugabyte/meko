// Add and rename a folder. Two buttons and the dialog behind them, kept
// together so the collection header and the Settings tab offer the same pair
// rather than each wiring its own.
//
// The header is where a user reaches for these while looking at the tree;
// Settings is where they look when they already know what they want. Both
// are legitimate, and neither should own the dialog.

import { useState } from "react";
import { Button } from "@/components/ui/button";
import type { CollectionFolder } from "@/lib/api/collectionsApi";
import { FolderNameDialog } from "@/features/collections/components/FolderNameDialog";
import { ADD_FOLDER_LABEL, RENAME_FOLDER_LABEL } from "@/features/collections/collectionsCopy";

export function CollectionFolderActions({
  collectionId,
  folders,
  isOwner,
  size = "sm",
  addVariant = "default",
}: {
  collectionId: string;
  folders: CollectionFolder[];
  /** A collection shared with you shows the tree but changes nothing in it. */
  isOwner: boolean;
  size?: "sm" | "default";
  /** Add folder is the primary action in the page header, where it is the
   *  main thing on offer. Inside Settings it is one row of several, so the
   *  card passes "secondary" rather than putting a third emphasis there. */
  addVariant?: "default" | "secondary";
}) {
  const [dialog, setDialog] = useState<"create" | "rename" | null>(null);

  // Hidden rather than disabled. A greyed-out button reads as "you could do
  // this if something were different", and for a shared collection nothing the
  // viewer can do will ever enable it.
  if (!isOwner) return null;

  return (
    <>
      {/* Rename first: secondary before primary, as the datapack rows do.
          Adding a folder is what someone reaches for here; renaming is
          housekeeping on a folder that already exists. */}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="secondary"
          size={size}
          disabled={folders.length === 0}
          onClick={() => setDialog("rename")}
        >
          {RENAME_FOLDER_LABEL}
        </Button>
        <Button
          type="button"
          variant={addVariant}
          size={size}
          onClick={() => setDialog("create")}
        >
          {ADD_FOLDER_LABEL}
        </Button>
      </div>

      <FolderNameDialog
        open={dialog !== null}
        onOpenChange={(open) => setDialog(open ? dialog : null)}
        collectionId={collectionId}
        folders={folders}
        mode={dialog ?? "create"}
      />
    </>
  );
}
