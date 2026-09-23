// Narrows a datapack list to one collection, or to the unfiled set. Pure, so the
// "unfiled means no placement row" rule is testable without a rendered grid.

import type { Datapack } from "@/lib/api/mekoApi";
import type { DatapackPathMap } from "@/lib/api/collectionsApi";

/** Filter value meaning "don't narrow at all". */
export const COLLECTION_FILTER_ALL = "all";
/** Filter value for datapacks with no placement in this user's tree. */
export const COLLECTION_FILTER_UNFILED = "unfiled";

/**
 * `paths` is keyed by datapack_id; ABSENCE from it is what makes a datapack
 * unfiled, so this needs no separate unfiled flag on the datapack itself.
 *
 * An unknown filter value returns the list unchanged rather than an empty
 * grid — a stale value from a deleted collection should not look like "you have
 * no datapacks".
 */
export function filterDatapacksByCollection(
  datapacks: Datapack[],
  paths: DatapackPathMap,
  filter: string,
): Datapack[] {
  if (filter === COLLECTION_FILTER_ALL) return datapacks;
  if (filter === COLLECTION_FILTER_UNFILED) {
    return datapacks.filter((dp) => paths[dp.datapack_id] === undefined);
  }
  const known = datapacks.filter((dp) => paths[dp.datapack_id]?.collection_id === filter);
  // A filter naming a collection that no longer has (or never had) any datapacks
  // is still a legitimate empty result; only an unrecognised shape falls back.
  return known;
}
