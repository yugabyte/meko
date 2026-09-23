// Pick a destination in the collection tree: a collection, then a folder inside it.
// Two selects rather than one flat list of every folder the user owns, because
// a folder name is only unique within its collection.
//
// Shared: used by the file/move dialog and by the create-datapack
// wizard, which need the same choice at different moments.

import { useEffect, useState } from "react";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useCollection, useCollections } from "@/features/collections/hooks/useCollections";

export function CollectionFolderSelect({
  folderId,
  onFolderChange,
  enabled = true,
  idPrefix,
  defaultCollectionId = "",
}: {
  /** Selected folder, or "" when nothing is chosen yet. */
  folderId: string;
  onFolderChange: (folderId: string) => void;
  /** Gates both queries, so a disabled picker fetches nothing. */
  enabled?: boolean;
  /** Distinguishes the two label/control pairs when more than one picker can
   *  exist on a page. */
  idPrefix: string;
  /** Preselects a collection, for a caller opening this from inside one. Still
   *  changeable: moving a datapack to a different collection is allowed. */
  defaultCollectionId?: string;
}) {
  const [collectionId, setCollectionId] = useState(defaultCollectionId);
  // Owned only: filing into someone else's collection is not a thing you can do.
  const { data: collectionLists } = useCollections({ enabled });
  const collections = collectionLists?.owned ?? [];
  // Only the chosen collection's tree is read — never every tree the user owns.
  const { data: detail, isLoading: foldersLoading } = useCollection(
    enabled && collectionId ? collectionId : undefined,
  );
  const folders = detail?.folders ?? [];

  // Changing collection invalidates any folder chosen under the previous one.
  useEffect(() => {
    onFolderChange("");
    // onFolderChange is the parent's setter; depending on it would re-fire
    // this whenever the parent re-renders with a new closure.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [collectionId]);

  return (
    <>
      <div className="space-y-2">
        <Label htmlFor={`${idPrefix}-collection`}>Collection</Label>
        <Select value={collectionId} onValueChange={setCollectionId} disabled={!enabled}>
          <SelectTrigger id={`${idPrefix}-collection`}>
            <SelectValue placeholder="Select a collection" />
          </SelectTrigger>
          <SelectContent>
            {collections.map((p) => (
              <SelectItem key={p.collection_id} value={p.collection_id}>
                {p.collection_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>

      <div className="space-y-2">
        <Label htmlFor={`${idPrefix}-folder`}>Folder</Label>
        <Select value={folderId} onValueChange={onFolderChange} disabled={!enabled || !collectionId}>
          <SelectTrigger id={`${idPrefix}-folder`}>
            <SelectValue
              placeholder={
                !collectionId
                  ? "Pick a collection first"
                  : foldersLoading
                    ? "Loading folders…"
                    : "Select a folder"
              }
            />
          </SelectTrigger>
          <SelectContent>
            {folders.map((folder) => (
              <SelectItem key={folder.folder_id} value={folder.folder_id}>
                {folder.folder_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
    </>
  );
}
