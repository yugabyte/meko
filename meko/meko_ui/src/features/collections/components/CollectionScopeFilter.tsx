// Collection filter for the Datapacks list: all collections, one collection, or the
// unfiled set. Sits beside the owned/shared tabs rather than replacing them —
// the two narrow on different axes (who can see it vs. where it lives).

import { Folder } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Collection } from "@/lib/api/collectionsApi";
import {
  COLLECTION_FILTER_ALL,
  COLLECTION_FILTER_UNFILED,
} from "@/features/collections/lib/filterDatapacksByCollection";
import { UNFILED_LABEL } from "@/features/collections/collectionsCopy";

export function CollectionScopeFilter({
  collections,
  value,
  onChange,
}: {
  collections: Collection[];
  value: string;
  onChange: (value: string) => void;
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger
        className="h-9 w-full sm:w-auto sm:min-w-[170px]"
        aria-label="Filter datapacks by collection"
      >
        <Folder className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={COLLECTION_FILTER_ALL}>All collections</SelectItem>
        <SelectItem value={COLLECTION_FILTER_UNFILED}>{UNFILED_LABEL}</SelectItem>
        {collections.length > 0 ? <SelectSeparator /> : null}
        {collections.map((collection) => (
          <SelectItem key={collection.collection_id} value={collection.collection_id}>
            {collection.collection_name}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  );
}
