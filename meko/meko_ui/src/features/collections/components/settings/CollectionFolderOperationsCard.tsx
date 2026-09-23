// The operations that reshape a collection: add or rename a folder, and move a
// datapack into one. Add and rename come from CollectionFolderActions, which the
// collection header also uses; moving is here only, because it needs a datapack
// picker rather than a folder one.

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import type { Datapack } from "@/lib/api/mekoApi";
import type { Collection, CollectionFolder } from "@/lib/api/collectionsApi";
import { CollectionFolderActions } from "@/features/collections/components/CollectionFolderActions";
import { FileIntoCollectionDialog } from "@/features/collections/components/FileIntoCollectionDialog";
import {
  DATAPACK_MOVED_TOAST,
  FOLDER_OPERATIONS_TITLE,
  MOVE_DATAPACK_LABEL,
} from "@/features/collections/collectionsCopy";

export function CollectionFolderOperationsCard({
  collection,
  folders,
  ownDatapacks,
}: {
  collection: Collection;
  folders: CollectionFolder[];
  /** Every datapack the signed-in user owns, for the move picker. */
  ownDatapacks: Datapack[];
}) {
  const [moveOpen, setMoveOpen] = useState(false);

  return (
    <Card className="border-border bg-card p-6">
      <h2 className="text-base font-semibold text-foreground">{FOLDER_OPERATIONS_TITLE}</h2>
      {/* All three secondary. This card is a list of operations rather than a
          place with one obvious next step, and the page header already carries
          the primary Add folder. Folder actions lead, then the datapack one. */}
      <div className="mt-3 flex flex-wrap items-center gap-2">
        <CollectionFolderActions
          collectionId={collection.collection_id}
          folders={folders}
          isOwner
          addVariant="secondary"
        />
        <Button
          type="button"
          variant="secondary"
          size="sm"
          disabled={folders.length === 0}
          onClick={() => setMoveOpen(true)}
        >
          {MOVE_DATAPACK_LABEL}
        </Button>
      </div>
      <p className="mt-3 text-sm text-muted-foreground">
        Moving a datapack you own into a folder here works whether it is unfiled or already in
        another collection. A datapack you do not own cannot be moved.
      </p>

      <FileIntoCollectionDialog
        open={moveOpen}
        onOpenChange={setMoveOpen}
        datapack={null}
        choices={ownDatapacks}
        title={MOVE_DATAPACK_LABEL}
        confirmLabel="Move here"
        successTitle={DATAPACK_MOVED_TOAST}
        defaultCollectionId={collection.collection_id}
      />
    </Card>
  );
}
