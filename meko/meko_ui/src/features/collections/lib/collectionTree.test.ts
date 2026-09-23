// The detail page's numbers are all sums over the datapack rows the collection
// response carried, so these are the arithmetic the page is built on.

import { describe, expect, it } from "vitest";
import type { CollectionDatapackNode } from "@/lib/api/collectionsApi";
import {
  EMPTY_TOTALS,
  isSameSelection,
  latestChange,
  nodesInFolder,
  sumNodes,
} from "./collectionTree";

const node = (over: Partial<CollectionDatapackNode>): CollectionDatapackNode => ({
  datapack_id: "d1",
  datapack_name: "perf",
  folder_id: "f1",
  updated_at: "2026-09-01T00:00:00Z",
  memory_count: 0,
  awaiting_review_count: 0,
  promoted_count: 0,
  knowledge_count: 0,
  ...over,
});

const NODES = [
  node({ datapack_id: "a", folder_id: "f1", memory_count: 10, awaiting_review_count: 3, promoted_count: 2, knowledge_count: 5 }),
  node({ datapack_id: "b", folder_id: "f1", memory_count: 1, awaiting_review_count: 1, promoted_count: 0, knowledge_count: 0 }),
  node({ datapack_id: "c", folder_id: "f2", memory_count: 4, awaiting_review_count: 0, promoted_count: 7, knowledge_count: 1 }),
];

describe("sumNodes", () => {
  it("adds every count across the set", () => {
    expect(sumNodes(NODES)).toEqual({
      datapacks: 3,
      memories: 15,
      awaitingReview: 4,
      promoted: 9,
      knowledge: 6,
    });
  });

  it("returns zeros for an empty collection rather than undefined", () => {
    expect(sumNodes([])).toEqual(EMPTY_TOTALS);
  });

  it("does not mutate the shared EMPTY_TOTALS", () => {
    sumNodes(NODES);
    expect(EMPTY_TOTALS.memories).toBe(0);
  });
});

describe("nodesInFolder", () => {
  it("keeps only the datapacks filed in that folder", () => {
    expect(nodesInFolder(NODES, "f1").map((n) => n.datapack_id)).toEqual(["a", "b"]);
  });

  it("is empty for a folder holding nothing", () => {
    expect(nodesInFolder(NODES, "f9")).toEqual([]);
  });
});

describe("latestChange", () => {
  it("returns the most recent timestamp", () => {
    const got = latestChange([
      node({ updated_at: "2026-01-01T00:00:00Z" }),
      node({ updated_at: "2026-09-20T00:00:00Z" }),
      node({ updated_at: "2026-05-05T00:00:00Z" }),
    ]);
    expect(got).toBe("2026-09-20T00:00:00Z");
  });

  it("is undefined for an empty folder, so the caller can say so", () => {
    expect(latestChange([])).toBeUndefined();
  });
});

describe("isSameSelection", () => {
  it("distinguishes two folders", () => {
    expect(
      isSameSelection({ kind: "folder", folderId: "f1" }, { kind: "folder", folderId: "f2" }),
    ).toBe(false);
  });

  it("matches the same folder", () => {
    expect(
      isSameSelection({ kind: "folder", folderId: "f1" }, { kind: "folder", folderId: "f1" }),
    ).toBe(true);
  });

  it("never matches across kinds", () => {
    expect(isSameSelection({ kind: "collection" }, { kind: "learnings" })).toBe(false);
  });

  it("matches the singleton kinds, which carry no id", () => {
    expect(isSameSelection({ kind: "learnings" }, { kind: "learnings" })).toBe(true);
  });
});
