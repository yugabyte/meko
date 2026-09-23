// One titled grid of collection cards — "Your collections" or "Shared collections".
// Both sections render identically, so the search results and empty states
// stay consistent between them.

import { Folder } from "lucide-react";
import type { CollectionMatch } from "@/features/collections/lib/filterCollections";
import type { Collection } from "@/lib/api/collectionsApi";
import { CollectionCard } from "@/features/collections/components/CollectionCard";

export function CollectionSection({
  title,
  subtitle,
  matches,
  learningsByCollection,
  emptyMessage,
  onOpen,
}: {
  title: string;
  subtitle?: string;
  matches: CollectionMatch[];
  learningsByCollection: Record<string, number>;
  /** Shown when a search hides everything in this section. Omit to render
   *  nothing at all, which is what an empty "Shared collections" should do. */
  emptyMessage?: string;
  onOpen: (collection: Collection) => void;
}) {
  if (matches.length === 0 && !emptyMessage) return null;

  return (
    <section className="space-y-4">
      <div className="space-y-1">
        <h2 className="text-lg font-semibold text-foreground">{title}</h2>
        {subtitle ? <p className="text-sm text-muted-foreground">{subtitle}</p> : null}
      </div>

      {matches.length === 0 ? (
        <p className="text-sm text-muted-foreground">{emptyMessage}</p>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {matches.map(({ collection, matchedFolders }) => (
            <div key={collection.collection_id} className="space-y-1">
              <CollectionCard
                collection={collection}
                learningsCount={learningsByCollection[collection.collection_id] ?? 0}
                onOpen={onOpen}
              />
              {/* Says WHY this card survived the search, so a match on a folder
                  doesn't look like a match on the collection name. */}
              {matchedFolders.length > 0 ? (
                <p className="flex items-center gap-1 px-1 text-xs text-muted-foreground">
                  <Folder className="h-3 w-3 shrink-0" aria-hidden />
                  matched {matchedFolders.join(", ")}
                </p>
              ) : null}
            </div>
          ))}
        </div>
      )}
    </section>
  );
}
