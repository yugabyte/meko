// react-query wrappers around the /projects surface: the project list, one
// project's folder tree, the theme registry, and the mutations that reshape a
// tree. Every mutation invalidates the keys its write can change, so the
// sidebar, the list and a detail page never disagree about the same tree.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { DATAPACKS_LIST_UPDATED_EVENT, deleteDatapack } from "@/lib/api/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { DATAPACKS_QUERY_KEY } from "@/features/datapacks/hooks/useDatapacks";
import {
  createProject,
  createProjectFolder,
  deleteProject,
  deleteProjectFolder,
  getProject,
  getProjectStats,
  listDatapackPaths,
  listProjects,
  listProjectThemes,
  placeDatapack,
  renameProjectFolder,
  unplaceDatapack,
  updateProject,
  type CreateProjectBody,
  type DatapackPathMap,
  type ProjectLists,
  type ProjectDetail,
  type ProjectStats,
  type ProjectTheme,
} from "@/lib/api/projectsApi";

/** Query keys. Exported so any surface can invalidate the same entries —
 *  a mutation elsewhere must hit these exact keys or the UI goes stale. */
export const PROJECTS_QUERY_KEY = "projects";
export const PROJECT_THEMES_QUERY_KEY = "project-themes";
export const DATAPACK_PATHS_QUERY_KEY = "datapack-paths";
export const PROJECT_STATS_QUERY_KEY = "project-stats";

/** Matches useDatapacks: long enough that navigating between surfaces inside
 *  a minute doesn't refetch, short enough that a new project appears on the
 *  next nav even if an invalidation were missed. */
const PROJECTS_STALE_TIME_MS = 30_000;

const EMPTY_LISTS: ProjectLists = { owned: [], shared: [] };

/** Both lists in one call. Callers that only want one destructure it. */
export function useProjects(options?: { enabled?: boolean }) {
  return useQuery<ProjectLists>({
    queryKey: [PROJECTS_QUERY_KEY],
    queryFn: () => listProjects(),
    staleTime: PROJECTS_STALE_TIME_MS,
    enabled: options?.enabled ?? true,
    placeholderData: EMPTY_LISTS,
  });
}

export function useProject(projectId: string | undefined) {
  return useQuery<ProjectDetail>({
    queryKey: [PROJECTS_QUERY_KEY, projectId],
    queryFn: () => getProject(projectId as string),
    staleTime: PROJECTS_STALE_TIME_MS,
    enabled: Boolean(projectId),
  });
}

/** The theme registry is a compile-time constant on the server, so it is
 *  fetched once and never refetched for the session. */
export function useProjectThemes(options?: { enabled?: boolean }) {
  return useQuery<ProjectTheme[]>({
    queryKey: [PROJECT_THEMES_QUERY_KEY],
    queryFn: () => listProjectThemes(),
    staleTime: Infinity,
    enabled: options?.enabled ?? true,
  });
}

/** Every filed datapack's path in one call, for labelling the Datapacks page.
 *  A datapack absent from the map is unfiled. */
export function useDatapackPaths(options?: { enabled?: boolean }) {
  return useQuery<DatapackPathMap>({
    queryKey: [DATAPACK_PATHS_QUERY_KEY],
    queryFn: () => listDatapackPaths(),
    staleTime: PROJECTS_STALE_TIME_MS,
    enabled: options?.enabled ?? true,
  });
}

/** Invalidates everything a tree-shape change can affect.
 *
 *  Paths are included even for a pure folder edit: a renamed folder changes
 *  the path string of every datapack filed beneath it, which the Datapacks
 *  page renders from the cached map. */
function useInvalidateTree() {
  const queryClient = useQueryClient();
  return () => {
    void queryClient.invalidateQueries({ queryKey: [PROJECTS_QUERY_KEY] });
    void queryClient.invalidateQueries({ queryKey: [DATAPACK_PATHS_QUERY_KEY] });
    // Stats are computed over the project's placed datapacks, so filing,
    // unfiling or deleting one changes them. Its key sits outside the two
    // prefixes above, so without this the memory tiles refetch from the
    // detail query while the rest keep the old datapack set and the panel
    // disagrees with itself until a focus refetch.
    void queryClient.invalidateQueries({ queryKey: [PROJECT_STATS_QUERY_KEY] });
  };
}

export function useCreateProject() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: CreateProjectBody) => createProject(body),
    onSuccess: invalidate,
  });
}

export function useUpdateProject(projectId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: { project_name: string; description?: string }) =>
      updateProject(projectId, body),
    onSuccess: invalidate,
  });
}

export function useDeleteProject() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (projectId: string) => deleteProject(projectId),
    onSuccess: invalidate,
  });
}

export function useCreateFolder(projectId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: { folder_name: string; folder_description?: string }) =>
      createProjectFolder(projectId, body),
    onSuccess: invalidate,
  });
}

export function useRenameFolder(projectId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (args: { folderId: string; folderName: string; description?: string }) =>
      renameProjectFolder(projectId, args.folderId, {
        folder_name: args.folderName,
        // Always sent: the server replaces the stored description, so
        // omitting it on a rename would clear it.
        folder_description: args.description ?? "",
      }),
    onSuccess: invalidate,
  });
}

export function useDeleteFolder(projectId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (folderId: string) => deleteProjectFolder(projectId, folderId),
    onSuccess: invalidate,
  });
}

/** Files a datapack into a folder, or moves it between folders. */
export function usePlaceDatapack() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (args: { datapackId: string; folderId: string }) =>
      placeDatapack(args.datapackId, args.folderId),
    onSuccess: invalidate,
  });
}

/** Returns a datapack to the unfiled set. */
export function useUnplaceDatapack() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (datapackId: string) => unplaceDatapack(datapackId),
    onSuccess: invalidate,
  });
}

/** Deletes a datapack from inside a project, then refreshes everything that
 *  was showing it: the tree, the path map, the datapack list, and the sidebar
 *  (which listens for the app event rather than sharing the query cache). */
export function useDeleteDatapackInTree() {
  const invalidate = useInvalidateTree();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (datapackId: string) => deleteDatapack(datapackId),
    onSuccess: () => {
      invalidate();
      void queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] });
      dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
    },
  });
}

/** The statistics panel. Its own query rather than part of useProject so
 *  the tree renders without waiting on four more aggregates. Memory counts
 *  are deliberately NOT here — they come from the detail query, which already
 *  pays the one scan that does fan out across 128 partitions. */
export function useProjectStats(projectId: string | undefined) {
  return useQuery<ProjectStats>({
    queryKey: [PROJECT_STATS_QUERY_KEY, projectId],
    queryFn: () => getProjectStats(projectId as string),
    staleTime: PROJECTS_STALE_TIME_MS,
    enabled: Boolean(projectId),
  });
}
