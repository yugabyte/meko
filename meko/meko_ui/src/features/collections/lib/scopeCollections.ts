// Which collections the All / Mine / Shared with me tabs show.
//
// Mirrors the Datapacks page's scope tabs so the two lists behave the same way,
// but the input differs: GET /collections already returns owned and shared as
// two arrays, so this narrows and concatenates rather than inspecting a grant
// on each row.

import type { Collection } from "@/lib/api/collectionsApi";

export const COLLECTION_SCOPE = {
  all: "all",
  owned: "owned",
  shared: "shared",
} as const;

export type CollectionScope = (typeof COLLECTION_SCOPE)[keyof typeof COLLECTION_SCOPE];

export function scopedCollections(
  owned: Collection[],
  shared: Collection[],
  scope: CollectionScope,
): Collection[] {
  switch (scope) {
    case COLLECTION_SCOPE.owned:
      return owned;
    case COLLECTION_SCOPE.shared:
      return shared;
    default:
      // Owned first: on the All tab your own collections are the ones you act
      // on, and a long shared list should not push them below the fold.
      return [...owned, ...shared];
  }
}

/** Empty-state copy that names the reason the grid is empty, rather than one
 *  message that is wrong in two of the three cases. */
export function emptyScopeMessage(scope: CollectionScope, searchActive: boolean): string {
  if (searchActive) return "No collections or folders match your search.";
  switch (scope) {
    case COLLECTION_SCOPE.owned:
      return "You have not created a collection yet.";
    case COLLECTION_SCOPE.shared:
      return "No collections have been shared with you.";
    default:
      return "No collections yet.";
  }
}
