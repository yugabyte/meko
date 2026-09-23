// Collections & folders API — the user-owned tree that organises datapacks, and
// where each datapack sits inside it. Kept out of mekoApi.ts as its own domain
// module; reuses mekoApi's `request` (envelope unwrap + error normalisation).
// Gated by MEKO_ENABLE_COLLECTIONS on both sides — see lib/collectionsFeature.ts.

import { request, type KBActivityLogResponse } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

const COLLECTIONS_ROUTE = "/collections";

function collectionRoute(collectionId: string): string {
  return `${COLLECTIONS_ROUTE}/${encodeURIComponent(collectionId)}`;
}

function foldersRoute(collectionId: string): string {
  return `${collectionRoute(collectionId)}/folders`;
}

function folderRoute(collectionId: string, folderId: string): string {
  return `${foldersRoute(collectionId)}/${encodeURIComponent(folderId)}`;
}

function placementRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/placement`;
}

/** A selectable folder layout. Served from api_server constants, not a table —
 *  a theme is stamped into a collection's folders at creation and never read
 *  again, so this list is the only source of truth for the picker. */
export type CollectionTheme = {
  key: string;
  label: string;
  description: string;
  version: string;
  folders: string[];
};

/** A collection: the top of one tree. `folder_count` / `datapack_count` are
 *  rollups the API fills from indexed counts, not stored columns. */
export type Collection = {
  collection_id: string;
  user_id: string;
  collection_name: string;
  theme_key: string;
  theme_version: string;
  description?: string;
  folder_count: number;
  datapack_count: number;
  /** The collection's folder names, so a card can show its shape without a detail
   *  call. On a SHARED collection these are scoped to folders holding a datapack
   *  you can reach. */
  folder_names?: string[];
  /** Only set on shared collections: whose collection this is. */
  owner_name?: string;
  created_at: string;
  updated_at: string;
};

/** A folder inside a collection. Folders do NOT nest: a collection holds folders, a
 *  folder holds datapacks. On a shared collection `datapack_count` counts only
 *  what the caller can see. */
export type CollectionFolder = {
  folder_id: string;
  collection_id: string;
  folder_name: string;
  datapack_count: number;
  created_at: string;
  updated_at: string;
};

/** One datapack inside a collection, with what it holds.
 *
 *  The counts arrive with the collection in a single response — the API groups
 *  them over every datapack at once rather than asking per datapack — so the
 *  detail page can show a folder's or the whole collection's totals by summing
 *  these rows, with no further requests. Conversations are absent on purpose:
 *  they live in a different database that can only answer one datapack at a
 *  time. */
export type CollectionDatapackNode = {
  datapack_id: string;
  datapack_name: string;
  folder_id: string;
  datapack_owner_name?: string;
  created_by_agent_id?: string;
  updated_at: string;
  /** Private memories held by this datapack. */
  memory_count: number;
  /** Memories with no promotion decision yet. A subset of memory_count. */
  awaiting_review_count: number;
  /** Memories promoted into the collection's shared knowledge. */
  promoted_count: number;
  /** Uploaded knowledge files. */
  knowledge_count: number;
};

/** Who is in this collection. The two numbers answer different questions and
 *  come from different places, so they are not comparable. */
export type CollectionMembers = {
  /** People who can reach at least one datapack here, the owner included. */
  people: number;
  /** Agents that have actually written into those datapacks, which is not
   *  the same as agents with an access record: only the default agent is
   *  ever registered, while the agents doing the work appear on the rows
   *  they wrote. */
  agents: number;
};

export type CollectionDetail = {
  collection: Collection;
  folders: CollectionFolder[];
  datapacks: CollectionDatapackNode[];
  members: CollectionMembers;
};

/** Where one datapack sits in the caller's tree. `path` is assembled
 *  server-side ("ADLC/features/perf") so every surface renders it the same. */
export type DatapackPath = {
  datapack_id: string;
  collection_id: string;
  collection_name: string;
  folder_id: string;
  /** The folder the datapack sits in, e.g. "features". */
  folder_name: string;
  /** Full label, always collection/folder, e.g. "adlc/features". */
  path: string;
};

/** Keyed by datapack_id. A datapack ABSENT from this map is unfiled — that is
 *  how the Datapacks page tells filed from unfiled without a second call. */
export type DatapackPathMap = Record<string, DatapackPath>;

export async function listCollectionThemes(options: RouteOptions = {}): Promise<CollectionTheme[]> {
  return request<CollectionTheme[]>(`${COLLECTIONS_ROUTE}/themes`, "GET", undefined, options);
}

/** Owned and shared collections come back separately: a recipient can do far less
 *  with a shared one, so keeping them apart stops the UI treating them alike.
 *  On a shared collection every count and folder name is scoped to what the
 *  caller can actually reach. */
export type CollectionLists = {
  owned: Collection[];
  shared: Collection[];
};

export async function listCollections(options: RouteOptions = {}): Promise<CollectionLists> {
  return request<CollectionLists>(COLLECTIONS_ROUTE, "GET", undefined, options);
}

export async function getCollection(
  collectionId: string,
  options: RouteOptions = {},
): Promise<CollectionDetail> {
  return request<CollectionDetail>(collectionRoute(collectionId), "GET", undefined, options);
}

export type CreateCollectionBody = {
  collection_name: string;
  description?: string;
  /** Omit to take the API's default theme. */
  theme_key?: string;
  /** Replaces the theme's own folder list. Sent for the Blank theme, which
   *  ships no layout and asks the user to name the first folder. */
  folders?: string[];
};

export async function createCollection(
  body: CreateCollectionBody,
  options: RouteOptions = {},
): Promise<Collection> {
  return request<Collection>(COLLECTIONS_ROUTE, "POST", body, options);
}

export async function updateCollection(
  collectionId: string,
  body: { collection_name: string; description?: string },
  options: RouteOptions = {},
): Promise<{ collection_id: string }> {
  return request<{ collection_id: string }>(collectionRoute(collectionId), "PATCH", body, options);
}

export async function deleteCollection(
  collectionId: string,
  options: RouteOptions = {},
): Promise<{ collection_id: string }> {
  return request<{ collection_id: string }>(collectionRoute(collectionId), "DELETE", undefined, options);
}

export async function createCollectionFolder(
  collectionId: string,
  body: { folder_name: string },
  options: RouteOptions = {},
): Promise<CollectionFolder> {
  return request<CollectionFolder>(foldersRoute(collectionId), "POST", body, options);
}

export async function renameCollectionFolder(
  collectionId: string,
  folderId: string,
  body: { folder_name: string },
  options: RouteOptions = {},
): Promise<{ folder_id: string }> {
  return request<{ folder_id: string }>(
    folderRoute(collectionId, folderId),
    "PATCH",
    body,
    options,
  );
}

export async function deleteCollectionFolder(
  collectionId: string,
  folderId: string,
  options: RouteOptions = {},
): Promise<{ folder_id: string }> {
  return request<{ folder_id: string }>(
    folderRoute(collectionId, folderId),
    "DELETE",
    undefined,
    options,
  );
}

/** Every filed datapack's path, in one call — enough to label the whole
 *  Datapacks page without a request per row. */
export async function listDatapackPaths(options: RouteOptions = {}): Promise<DatapackPathMap> {
  return request<DatapackPathMap>(`${COLLECTIONS_ROUTE}/placements`, "GET", undefined, options);
}

/** Files a datapack into a folder, or moves it if already filed (upsert).
 *  Owner-only: a datapack has one home, in a collection its owner controls, so a
 *  recipient of a share cannot relocate it. */
export async function placeDatapack(
  datapackId: string,
  folderId: string,
  options: RouteOptions = {},
): Promise<{ datapack_id: string }> {
  return request<{ datapack_id: string }>(
    placementRoute(datapackId),
    "POST",
    { folder_id: folderId },
    options,
  );
}

/** Returns a datapack to the unfiled set. Owner-only. */
export async function unplaceDatapack(
  datapackId: string,
  options: RouteOptions = {},
): Promise<{ datapack_id: string }> {
  return request<{ datapack_id: string }>(
    placementRoute(datapackId),
    "DELETE",
    undefined,
    options,
  );
}

/** One page of the collection's activity feed: file uploads and memory
 *  promotions across every datapack in the collection you can see, newest
 *  first.
 *
 *  Same envelope as the per-datapack feed, so the panel renders both. The
 *  cursor is opaque: pass back exactly what the server returned.
 *
 *  It reports those two things and nothing else. Nothing in Meko records
 *  that a datapack was created, a folder renamed or a memory written, so a
 *  feed cannot show them, and a deleted file takes its upload row with it. */
export async function listCollectionActivity(
  collectionId: string,
  args: { limit: number; cursor: string },
  options: RouteOptions = {},
): Promise<KBActivityLogResponse> {
  const params = new URLSearchParams({ limit: String(args.limit) });
  if (args.cursor) params.set("cursor", args.cursor);
  return request<KBActivityLogResponse>(
    `${collectionRoute(collectionId)}/activity-log?${params.toString()}`,
    "GET",
    undefined,
    options,
  );
}
