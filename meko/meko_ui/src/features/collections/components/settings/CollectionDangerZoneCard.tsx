// The two deletes that destroy content rather than structure, and the
// type-the-name guard in front of each.
//
// Both are owner-only, which the Settings tab enforces by not rendering for
// anyone else. Only datapacks the caller OWNS are offered: a shared datapack
// filed here belongs to someone else's account.

import { useState } from "react";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { ConfirmByTypingDialog } from "@/components/ui/confirm-by-typing-dialog";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Datapack } from "@/lib/api/mekoApi";
import type { Collection, CollectionDatapackNode } from "@/lib/api/collectionsApi";
import {
  useDeleteDatapackInTree,
  useDeleteCollection,
} from "@/features/collections/hooks/useCollections";
import {
  DANGER_ZONE_BODY,
  DANGER_ZONE_TITLE,
  DELETE_DATAPACK_LABEL,
  DELETE_COLLECTION_BODY,
  DELETE_COLLECTION_LABEL,
  deleteCollectionDatapackWarning,
  COLLECTION_TOASTS,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/collections/collectionsCopy";

export function CollectionDangerZoneCard({
  collection,
  datapacks,
  ownDatapacks,
  onCollectionDeleted,
}: {
  collection: Collection;
  /** Every datapack in this collection, for the delete count and the picker. */
  datapacks: CollectionDatapackNode[];
  ownDatapacks: Datapack[];
  onCollectionDeleted: () => void;
}) {
  const [deleteCollectionOpen, setDeleteCollectionOpen] = useState(false);
  const [deleteDatapackOpen, setDeleteDatapackOpen] = useState(false);
  const [doomedId, setDoomedId] = useState("");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const deleteCollectionMut = useDeleteCollection();
  const deleteDatapackMut = useDeleteDatapackInTree();

  const ownedIds = new Set(ownDatapacks.map((d) => d.datapack_id));
  const deletable = datapacks.filter((d) => ownedIds.has(d.datapack_id));
  const doomed = deletable.find((d) => d.datapack_id === doomedId);

  const handleDeleteCollection = async () => {
    setPending(true);
    setError(null);
    try {
      await deleteCollectionMut.mutateAsync(collection.collection_id);
      setDeleteCollectionOpen(false);
      onCollectionDeleted();
      // Navigates away on success, so the toast is the only confirmation
      // that survives the page change.
      toast({ title: COLLECTION_TOASTS.collectionDeleted.ok, description: collection.collection_name });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: COLLECTION_TOASTS.collectionDeleted.fail,
        description: message,
      });
    } finally {
      setPending(false);
    }
  };

  const handleDeleteDatapack = async () => {
    if (!doomed) return;
    setPending(true);
    setError(null);
    try {
      await deleteDatapackMut.mutateAsync(doomed.datapack_id);
      setDeleteDatapackOpen(false);
      setDoomedId("");
      toast({ title: COLLECTION_TOASTS.datapackDeleted.ok, description: doomed.datapack_name });
    } catch (e) {
      const message = getApiErrorMessage(e, TOAST_RETRY_DESCRIPTION);
      setError(message);
      toast({
        variant: "destructive",
        title: COLLECTION_TOASTS.datapackDeleted.fail,
        description: message,
      });
    } finally {
      setPending(false);
    }
  };

  /** Shown inside both dialogs, so a failure lands where the user is looking. */
  const errorLine = error ? (
    <span role="alert" className="mt-2 block text-destructive">
      {error}
    </span>
  ) : null;

  return (
    <Card className="border-destructive/40 bg-card p-6">
      <h2 className="text-base font-semibold text-destructive">{DANGER_ZONE_TITLE}</h2>
      <p className="mt-2 text-sm text-muted-foreground">{DANGER_ZONE_BODY}</p>
      <div className="mt-4 flex flex-wrap gap-2">
        <Button
          type="button"
          variant="destructive"
          size="sm"
          disabled={deletable.length === 0}
          onClick={() => {
            setDoomedId("");
            setError(null);
            setDeleteDatapackOpen(true);
          }}
        >
          {DELETE_DATAPACK_LABEL}
        </Button>
        <Button
          type="button"
          variant="destructive"
          size="sm"
          onClick={() => {
            setError(null);
            setDeleteCollectionOpen(true);
          }}
        >
          {DELETE_COLLECTION_LABEL}
        </Button>
      </div>

      <ConfirmByTypingDialog
        open={deleteCollectionOpen}
        onOpenChange={setDeleteCollectionOpen}
        title={`Delete ${collection.collection_name}?`}
        description={
          <>
            {DELETE_COLLECTION_BODY}{" "}
            <span className="font-medium text-foreground">
              {deleteCollectionDatapackWarning(datapacks.length)}
            </span>{" "}
            Type the collection name to confirm.
            {errorLine}
          </>
        }
        confirmPhrase={collection.collection_name}
        confirmLabel="Delete collection"
        pending={pending}
        onConfirm={handleDeleteCollection}
      />

      <ConfirmByTypingDialog
        open={deleteDatapackOpen}
        onOpenChange={setDeleteDatapackOpen}
        title="Delete a datapack?"
        description={
          <>
            The datapack and everything in it are removed for good: its memories, its knowledge
            and its history. Pick one, then type its name to confirm.
            {errorLine}
          </>
        }
        confirmPhrase={doomed?.datapack_name ?? ""}
        confirmLabel="Delete datapack"
        pending={pending}
        onConfirm={handleDeleteDatapack}
      >
        <Label htmlFor="danger-datapack">Datapack</Label>
        <Select value={doomedId} onValueChange={setDoomedId}>
          <SelectTrigger id="danger-datapack">
            <SelectValue placeholder="Select a datapack" />
          </SelectTrigger>
          <SelectContent>
            {deletable.map((d) => (
              <SelectItem key={d.datapack_id} value={d.datapack_id}>
                {d.datapack_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </ConfirmByTypingDialog>
    </Card>
  );
}
