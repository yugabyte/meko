// Searching and paging the datapacks inside one folder.
//
// All of it runs in the browser: GET /projects/:id already returns every
// datapack in the project with its counts, so filtering and paging a
// folder costs no request and no index. Pure, so the paging arithmetic can be
// tested without mounting a tree.

import type { ProjectDatapackNode } from "@/lib/api/projectsApi";

/** How many datapacks a folder shows before paging. Small on purpose: the
 *  point of the pane is to see what is in a folder at a glance, and a folder
 *  with thirty datapacks should not push everything else off the screen. */
export const FOLDER_PAGE_SIZE = 5;

export type FolderPage = {
  /** The rows to render for the current page. */
  rows: ProjectDatapackNode[];
  /** Matches across every page, which is what the count beside the heading
   *  should say — not the length of `rows`. */
  matchCount: number;
  pageCount: number;
  /** Clamped: a filter that shortens the list must not strand the viewer on
   *  a page that no longer exists. */
  page: number;
};

export function folderContents(
  nodes: ProjectDatapackNode[],
  folderId: string,
  query: string,
  page: number,
  pageSize: number = FOLDER_PAGE_SIZE,
): FolderPage {
  const inFolder = nodes.filter((n) => n.folder_id === folderId);
  const q = query.trim().toLowerCase();
  const matches =
    q === "" ? inFolder : inFolder.filter((n) => n.datapack_name.toLowerCase().includes(q));

  const pageCount = Math.max(1, Math.ceil(matches.length / pageSize));
  const clamped = Math.min(Math.max(page, 1), pageCount);
  const start = (clamped - 1) * pageSize;

  return {
    rows: matches.slice(start, start + pageSize),
    matchCount: matches.length,
    pageCount,
    page: clamped,
  };
}

/** Datapacks the user owns that are NOT already in this folder, for the
 *  add-datapack picker.
 *
 *  Owned only: placement refuses anything else, so offering a datapack
 *  belonging to someone else is offering a guaranteed error. Already-here
 *  ones are excluded rather than shown checked, because re-filing a datapack
 *  into the folder it is already in is a no-op the user did not ask for. */
export function addableDatapacks<T extends { datapack_id: string; datapack_name: string }>(
  owned: T[],
  nodes: ProjectDatapackNode[],
  folderId: string,
  query: string,
): T[] {
  const alreadyHere = new Set(
    nodes.filter((n) => n.folder_id === folderId).map((n) => n.datapack_id),
  );
  const q = query.trim().toLowerCase();
  return owned.filter(
    (d) =>
      !alreadyHere.has(d.datapack_id) && (q === "" || d.datapack_name.toLowerCase().includes(q)),
  );
}
