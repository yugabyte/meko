// "Unfiled" is defined by ABSENCE from the paths map, not by a flag on the
// datapack — these cover that rule and the filter values around it.

import { describe, expect, it } from "vitest";
import type { Datapack } from "@/lib/api/mekoApi";
import type { DatapackPathMap } from "@/lib/api/collectionsApi";
import {
  COLLECTION_FILTER_ALL,
  COLLECTION_FILTER_UNFILED,
  filterDatapacksByCollection,
} from "./filterDatapacksByCollection";

const dp = (id: string): Datapack =>
  ({ datapack_id: id, datapack_name: id, user_id: "u-1" }) as Datapack;

const FILED = dp("filed");
const ALSO_FILED = dp("also-filed");
const UNFILED = dp("unfiled");
const ALL = [FILED, ALSO_FILED, UNFILED];

const path = (datapackId: string, collectionId: string) => ({
  datapack_id: datapackId,
  collection_id: collectionId,
  collection_name: collectionId,
  folder_id: `${datapackId}-folder`,
  folder_name: "features",
  path: `${collectionId}/features`,
});

const PATHS: DatapackPathMap = {
  filed: path("filed", "proj-1"),
  "also-filed": path("also-filed", "proj-2"),
};

describe("filterDatapacksByCollection", () => {
  it("returns everything for the all filter", () => {
    expect(filterDatapacksByCollection(ALL, PATHS, COLLECTION_FILTER_ALL)).toEqual(ALL);
  });

  it("returns only datapacks missing from the paths map when filtering unfiled", () => {
    expect(filterDatapacksByCollection(ALL, PATHS, COLLECTION_FILTER_UNFILED)).toEqual([UNFILED]);
  });

  it("narrows to one collection", () => {
    expect(filterDatapacksByCollection(ALL, PATHS, "proj-1")).toEqual([FILED]);
  });

  it("treats an empty paths map as everything being unfiled", () => {
    expect(filterDatapacksByCollection(ALL, {}, COLLECTION_FILTER_UNFILED)).toEqual(ALL);
    expect(filterDatapacksByCollection(ALL, {}, "proj-1")).toEqual([]);
  });

  it("returns an empty list for a collection with nothing filed into it", () => {
    expect(filterDatapacksByCollection(ALL, PATHS, "proj-empty")).toEqual([]);
  });
});
