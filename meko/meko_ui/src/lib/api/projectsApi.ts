// Projects & folders API — the user-owned tree that organises datapacks, and
// where each datapack sits inside it. Kept out of mekoApi.ts as its own domain
// module; reuses mekoApi's `request` (envelope unwrap + error normalisation).
// Gated by MEKO_ENABLE_PROJECTS on both sides — see lib/projectsFeature.ts.

import { request, type KBActivityLogResponse } from "@/lib/api/mekoApi";

type RouteOptions = { signal?: AbortSignal };

const PROJECTS_ROUTE = "/projects";

function projectRoute(projectId: string): string {
  return `${PROJECTS_ROUTE}/${encodeURIComponent(projectId)}`;
}

function foldersRoute(projectId: string): string {
  return `${projectRoute(projectId)}/folders`;
}

function folderRoute(projectId: string, folderId: string): string {
  return `${foldersRoute(projectId)}/${encodeURIComponent(folderId)}`;
}

function placementRoute(datapackId: string): string {
  return `/datapacks/${encodeURIComponent(datapackId)}/placement`;
}

/** A selectable folder layout. Served from api_server constants, not a table —
 *  a theme is stamped into a project's folders at creation and never read
 *  again, so this list is the only source of truth for the picker. */
export type ProjectTheme = {
  key: string;
  label: string;
  description: string;
  version: string;
  folders: string[];
};

/** A project: the top of one tree. `folder_count` / `datapack_count` are
 *  rollups the API fills from indexed counts, not stored columns. */
export type Project = {
  project_id: string;
  user_id: string;
  project_name: string;
  theme_key: string;
  theme_version: string;
  description?: string;
  folder_count: number;
  datapack_count: number;
  /** The project's folder names, so a card can show its shape without a detail
   *  call. On a SHARED project these are scoped to folders holding a datapack
   *  you can reach. */
  folder_names?: string[];
  /** Only set on shared projects: whose project this is. */
  owner_name?: string;
  created_at: string;
  updated_at: string;
};

/** A folder inside a project. Folders do NOT nest: a project holds folders, a
 *  folder holds datapacks. On a shared project `datapack_count` counts only
 *  what the caller can see. */
export type ProjectFolder = {
  folder_id: string;
  project_id: string;
  folder_name: string;
  /** What the folder is for. Optional; absent or "" on folders created
   *  before descriptions existed. */
  folder_description?: string;
  datapack_count: number;
  created_at: string;
  updated_at: string;
};

/** One datapack inside a project, with what it holds.
 *
 *  The counts arrive with the project in a single response — the API groups
 *  them over every datapack at once rather than asking per datapack — so the
 *  detail page can show a folder's or the whole project's totals by summing
 *  these rows, with no further requests. Conversations are absent on purpose:
 *  they live in a different database that can only answer one datapack at a
 *  time. */
export type ProjectDatapackNode = {
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
  /** Memories promoted into the project's shared knowledge. */
  promoted_count: number;
  /** Uploaded knowledge files. */
  knowledge_count: number;
};

/** Who is in this project. The two numbers answer different questions and
 *  come from different places, so they are not comparable. */
export type ProjectMembers = {
  /** People who can reach at least one datapack here, the owner included. */
  people: number;
  /** Agents that have actually written into those datapacks, which is not
   *  the same as agents with an access record: only the default agent is
   *  ever registered, while the agents doing the work appear on the rows
   *  they wrote. */
  agents: number;
};

/** What the project holds right now: standing totals, no date range.
 *  Memory counts come from the detail response instead, so the tile and the
 *  tree sum the same rows. */
export type ProjectStats = {
  searches_hybrid: number;
  searches_kb: number;
  searches_memory: number;
  conversations: number;
  kb_files: number;
  kb_chunks: number;
  artifacts: number;
  artifact_bytes: number;
  kb_files_by_type: Record<string, number>;
};

export type ProjectDetail = {
  project: Project;
  folders: ProjectFolder[];
  datapacks: ProjectDatapackNode[];
  members: ProjectMembers;
};

/** Where one datapack sits in the caller's tree. `path` is assembled
 *  server-side ("ADLC/features/perf") so every surface renders it the same. */
export type DatapackPath = {
  datapack_id: string;
  project_id: string;
  project_name: string;
  folder_id: string;
  /** The folder the datapack sits in, e.g. "features". */
  folder_name: string;
  /** Full label, always project/folder, e.g. "adlc/features". */
  path: string;
};

/** Keyed by datapack_id. A datapack ABSENT from this map is unfiled — that is
 *  how the Datapacks page tells filed from unfiled without a second call. */
export type DatapackPathMap = Record<string, DatapackPath>;

export async function listProjectThemes(options: RouteOptions = {}): Promise<ProjectTheme[]> {
  return request<ProjectTheme[]>(`${PROJECTS_ROUTE}/themes`, "GET", undefined, options);
}

/** Owned and shared projects come back separately: a recipient can do far less
 *  with a shared one, so keeping them apart stops the UI treating them alike.
 *  On a shared project every count and folder name is scoped to what the
 *  caller can actually reach. */
export type ProjectLists = {
  owned: Project[];
  shared: Project[];
};

export async function listProjects(options: RouteOptions = {}): Promise<ProjectLists> {
  return request<ProjectLists>(PROJECTS_ROUTE, "GET", undefined, options);
}

export async function getProject(
  projectId: string,
  options: RouteOptions = {},
): Promise<ProjectDetail> {
  return request<ProjectDetail>(projectRoute(projectId), "GET", undefined, options);
}

export type CreateProjectBody = {
  project_name: string;
  description?: string;
  /** Omit to take the API's default theme. */
  theme_key?: string;
  /** Replaces the theme's own folder list. Sent for the Blank theme, which
   *  ships no layout and asks the user to name the first folder. */
  folders?: string[];
};

export async function createProject(
  body: CreateProjectBody,
  options: RouteOptions = {},
): Promise<Project> {
  return request<Project>(PROJECTS_ROUTE, "POST", body, options);
}

export async function updateProject(
  projectId: string,
  body: { project_name: string; description?: string },
  options: RouteOptions = {},
): Promise<{ project_id: string }> {
  return request<{ project_id: string }>(projectRoute(projectId), "PATCH", body, options);
}

export async function deleteProject(
  projectId: string,
  options: RouteOptions = {},
): Promise<{ project_id: string }> {
  return request<{ project_id: string }>(projectRoute(projectId), "DELETE", undefined, options);
}

export async function createProjectFolder(
  projectId: string,
  body: { folder_name: string; folder_description?: string },
  options: RouteOptions = {},
): Promise<ProjectFolder> {
  return request<ProjectFolder>(foldersRoute(projectId), "POST", body, options);
}

export async function renameProjectFolder(
  projectId: string,
  folderId: string,
  /** folder_description replaces the stored one outright, so omitting it on
   *  a rename would silently clear it. Callers send both. */
  body: { folder_name: string; folder_description?: string },
  options: RouteOptions = {},
): Promise<{ folder_id: string }> {
  return request<{ folder_id: string }>(folderRoute(projectId, folderId), "PATCH", body, options);
}

export async function deleteProjectFolder(
  projectId: string,
  folderId: string,
  options: RouteOptions = {},
): Promise<{ folder_id: string }> {
  return request<{ folder_id: string }>(
    folderRoute(projectId, folderId),
    "DELETE",
    undefined,
    options,
  );
}

/** Every filed datapack's path, in one call — enough to label the whole
 *  Datapacks page without a request per row. */
export async function listDatapackPaths(options: RouteOptions = {}): Promise<DatapackPathMap> {
  return request<DatapackPathMap>(`${PROJECTS_ROUTE}/placements`, "GET", undefined, options);
}

/** Files a datapack into a folder, or moves it if already filed (upsert).
 *  Owner-only: a datapack has one home, in a project its owner controls, so a
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
  return request<{ datapack_id: string }>(placementRoute(datapackId), "DELETE", undefined, options);
}

/** One page of the project's activity feed: file uploads and memory
 *  promotions across every datapack in the project you can see, newest
 *  first.
 *
 *  Same envelope as the per-datapack feed, so the panel renders both. The
 *  cursor is opaque: pass back exactly what the server returned.
 *
 *  It reports those two things and nothing else. Nothing in Meko records
 *  that a datapack was created, a folder renamed or a memory written, so a
 *  feed cannot show them, and a deleted file takes its upload row with it. */
export async function listProjectActivity(
  projectId: string,
  args: { limit: number; cursor: string },
  options: RouteOptions = {},
): Promise<KBActivityLogResponse> {
  const params = new URLSearchParams({ limit: String(args.limit) });
  if (args.cursor) params.set("cursor", args.cursor);
  return request<KBActivityLogResponse>(
    `${projectRoute(projectId)}/activity-log?${params.toString()}`,
    "GET",
    undefined,
    options,
  );
}

/** Standing totals for one project. No date range: every figure counts
 *  what the project holds now, so there is nothing for a window to narrow. */
export async function getProjectStats(
  projectId: string,
  options: RouteOptions = {},
): Promise<ProjectStats> {
  return request<ProjectStats>(`${projectRoute(projectId)}/stats`, "GET", undefined, options);
}
