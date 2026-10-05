// What is selected in the project tree, and the arithmetic behind the panel
// that describes it.
//
// Every total on the detail page is a sum over the datapack rows the project
// response already carried, so selecting a folder, the project or the
// learnings node issues no request. Pure functions, kept out of the
// components so the sums can be tested without rendering a tree.

import type { ProjectDatapackNode } from "@/lib/api/projectsApi";

/** The four things a row in the tree can be. `learnings` is a synthetic node,
 *  not a folder: it rolls up the whole project rather than holding anything. */
export type TreeSelection =
  | { kind: "project" }
  | { kind: "folder"; folderId: string }
  | { kind: "datapack"; datapackId: string }
  | { kind: "learnings" };

export const PROJECT_SELECTION: TreeSelection = { kind: "project" };
export const LEARNINGS_SELECTION: TreeSelection = { kind: "learnings" };

/** True when these two selections point at the same row. */
export function isSameSelection(a: TreeSelection, b: TreeSelection): boolean {
  if (a.kind !== b.kind) return false;
  if (a.kind === "folder" && b.kind === "folder") return a.folderId === b.folderId;
  if (a.kind === "datapack" && b.kind === "datapack") return a.datapackId === b.datapackId;
  return true;
}

export type ProjectTotals = {
  datapacks: number;
  memories: number;
  awaitingReview: number;
  promoted: number;
  knowledge: number;
};

export const EMPTY_TOTALS: ProjectTotals = {
  datapacks: 0,
  memories: 0,
  awaitingReview: 0,
  promoted: 0,
  knowledge: 0,
};

/** Rolls a set of datapacks into one set of numbers. */
export function sumNodes(nodes: ProjectDatapackNode[]): ProjectTotals {
  return nodes.reduce<ProjectTotals>(
    (acc, n) => ({
      datapacks: acc.datapacks + 1,
      memories: acc.memories + n.memory_count,
      awaitingReview: acc.awaitingReview + n.awaiting_review_count,
      promoted: acc.promoted + n.promoted_count,
      knowledge: acc.knowledge + n.knowledge_count,
    }),
    { ...EMPTY_TOTALS },
  );
}

export function nodesInFolder(
  nodes: ProjectDatapackNode[],
  folderId: string,
): ProjectDatapackNode[] {
  return nodes.filter((n) => n.folder_id === folderId);
}

/** The most recent `updated_at` in a set, or undefined when it is empty.
 *  Compared as ISO strings, which sort chronologically. */
export function latestChange(nodes: ProjectDatapackNode[]): string | undefined {
  return nodes.reduce<string | undefined>(
    (latest, n) => (latest === undefined || n.updated_at > latest ? n.updated_at : latest),
    undefined,
  );
}
