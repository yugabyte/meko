// Rolls each datapack's pending-learnings count up to the collection it is filed
// into, so a collection card can show one number.
//
// NOTE: `learnings_count` is only populated by the datapack DESCRIBE endpoint,
// never by the list — so every value here is 0 until MEKO-653 enriches the
// list response. The rollup is written against the real field rather than a
// placeholder, so it starts reporting the moment that lands.

import type { Datapack } from "@/lib/api/mekoApi";
import type { DatapackPathMap } from "@/lib/api/collectionsApi";

/** Keyed by collection_id. A collection with no filed datapacks is absent, which
 *  callers read as 0. */
export function collectionLearningsByCollection(
  datapacks: Datapack[],
  paths: DatapackPathMap,
): Record<string, number> {
  const out: Record<string, number> = {};
  for (const dp of datapacks) {
    const placement = paths[dp.datapack_id];
    if (!placement) continue;
    out[placement.collection_id] = (out[placement.collection_id] ?? 0) + (dp.learnings_count ?? 0);
  }
  return out;
}

/** Datapacks with no placement in the caller's tree. */
export function unfiledDatapacks(datapacks: Datapack[], paths: DatapackPathMap): Datapack[] {
  return datapacks.filter((dp) => paths[dp.datapack_id] === undefined);
}
