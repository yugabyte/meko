// Narrows a collection list by a search term, matching the collection's name OR any
// of its folder names.
//
// Runs in the browser against data the page already has: GET /collections returns
// folder_names on every collection, so folder search costs no request and needs no
// index. It is also leak-safe on shared collections, because folder_names there is
// already scoped to folders the caller can reach.

import type { Collection } from "@/lib/api/collectionsApi";

/** Whether this collection matches, and why — so the UI can show the folder that
 *  caused a match rather than leaving the user guessing. */
export type CollectionMatch = {
  collection: Collection;
  /** Folder names that matched. Empty when only the collection name did. */
  matchedFolders: string[];
};

export function filterCollections(collections: Collection[], query: string): CollectionMatch[] {
  const needle = query.trim().toLowerCase();
  if (!needle) return collections.map((collection) => ({ collection, matchedFolders: [] }));

  const out: CollectionMatch[] = [];
  for (const collection of collections) {
    const matchedFolders = (collection.folder_names ?? []).filter((name) =>
      name.toLowerCase().includes(needle),
    );
    const nameMatches = collection.collection_name.toLowerCase().includes(needle);
    if (nameMatches || matchedFolders.length > 0) {
      out.push({ collection, matchedFolders });
    }
  }
  return out;
}
