// Searching by FOLDER name is the point of this helper — a user remembers
// "features", not which collection it was in.

import { describe, expect, it } from "vitest";
import type { Collection } from "@/lib/api/collectionsApi";
import { filterCollections } from "./filterCollections";

const collection = (name: string, folders: string[]): Collection =>
  ({ collection_id: name, collection_name: name, folder_names: folders }) as Collection;

const ADLC = collection("adlc", ["coding_standards", "features", "integration_tests"]);
const SUPPORT = collection("support", ["playbooks", "escalations"]);
const ALL = [ADLC, SUPPORT];

describe("filterCollections", () => {
  it("returns everything for an empty query", () => {
    expect(filterCollections(ALL, "  ")).toHaveLength(2);
  });

  it("matches on the collection name", () => {
    const got = filterCollections(ALL, "support");
    expect(got.map((m) => m.collection.collection_name)).toEqual(["support"]);
  });

  it("matches on a folder name and reports which folder matched", () => {
    const got = filterCollections(ALL, "features");
    expect(got).toHaveLength(1);
    expect(got[0].collection.collection_name).toBe("adlc");
    expect(got[0].matchedFolders).toEqual(["features"]);
  });

  it("is case-insensitive and matches partials", () => {
    expect(filterCollections(ALL, "ESCAL")).toHaveLength(1);
  });

  it("reports every folder that matched, not just the first", () => {
    const got = filterCollections([collection("p", ["qa-unit", "qa-e2e"])], "qa");
    expect(got[0].matchedFolders).toEqual(["qa-unit", "qa-e2e"]);
  });

  it("leaves matchedFolders empty when only the collection name matched", () => {
    expect(filterCollections(ALL, "adlc")[0].matchedFolders).toEqual([]);
  });

  it("returns nothing when neither name nor folders match", () => {
    expect(filterCollections(ALL, "nope")).toEqual([]);
  });

  it("tolerates a collection with no folders", () => {
    expect(filterCollections([collection("empty", [])], "empty")).toHaveLength(1);
  });
});
