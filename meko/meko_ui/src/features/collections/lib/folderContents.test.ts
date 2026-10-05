// Paging is where a folder pane goes wrong: a filter that shortens the list
// can strand the viewer on an empty page, and the count beside the heading
// can quietly become "rows on this page" instead of "matches".

import { describe, expect, it } from "vitest";
import type { CollectionDatapackNode } from "@/lib/api/collectionsApi";
import { addableDatapacks, folderContents } from "@/features/collections/lib/folderContents";

const node = (id: string, folder: string, name = id): CollectionDatapackNode =>
  ({
    datapack_id: id,
    datapack_name: name,
    folder_id: folder,
    updated_at: "2026-01-01T00:00:00Z",
    memory_count: 0,
    awaiting_review_count: 0,
    promoted_count: 0,
    knowledge_count: 0,
  }) as CollectionDatapackNode;

// Twelve in f1 so the 5-per-page cap gives three pages, one of them partial.
const nodes = [
  ...Array.from({ length: 12 }, (_, i) => node(`dp-${i}`, "f1", `alpha-${i}`)),
  node("other", "f2", "beta"),
];

describe("folderContents", () => {
  it("shows only the folder asked for", () => {
    expect(folderContents(nodes, "f2", "", 1).matchCount).toBe(1);
  });

  it("pages at five and reports total matches, not rows on this page", () => {
    const p = folderContents(nodes, "f1", "", 1);
    expect(p.rows).toHaveLength(5);
    expect(p.matchCount).toBe(12);
    expect(p.pageCount).toBe(3);
  });

  it("returns the remainder on the last page", () => {
    expect(folderContents(nodes, "f1", "", 3).rows).toHaveLength(2);
  });

  it("clamps a page past the end back to the last real one", () => {
    // What happens when a filter shortens the list while the viewer is deep
    // in it: without the clamp they get a blank pane and no way back.
    const p = folderContents(nodes, "f1", "alpha-1", 3);
    expect(p.page).toBe(1);
    expect(p.rows.length).toBeGreaterThan(0);
  });

  it("filters by name, case-insensitively", () => {
    expect(folderContents(nodes, "f1", "ALPHA-7", 1).matchCount).toBe(1);
    expect(folderContents(nodes, "f1", "nothing", 1).matchCount).toBe(0);
  });

  it("never reports page 0, even with no matches", () => {
    const p = folderContents(nodes, "f1", "nothing", 1);
    expect(p.page).toBe(1);
    expect(p.pageCount).toBe(1);
  });
});

describe("addableDatapacks", () => {
  const owned = [
    { datapack_id: "dp-0", datapack_name: "alpha-0" },
    { datapack_id: "free", datapack_name: "unfiled-one" },
  ];

  it("hides datapacks already in this folder", () => {
    const out = addableDatapacks(owned, nodes, "f1", "");
    expect(out.map((d) => d.datapack_id)).toEqual(["free"]);
  });

  it("offers one that sits in a DIFFERENT folder, since filing moves it", () => {
    const out = addableDatapacks(owned, nodes, "f2", "");
    expect(out.map((d) => d.datapack_id)).toContain("dp-0");
  });

  it("searches by name", () => {
    expect(addableDatapacks(owned, nodes, "f2", "unfiled")).toHaveLength(1);
  });
});
