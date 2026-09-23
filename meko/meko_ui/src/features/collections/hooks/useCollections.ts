// react-query wrappers around the /collections surface: the collection list, one
// collection's folder tree, the theme registry, and the mutations that reshape a
// tree. Every mutation invalidates the keys its write can change, so the
// sidebar, the list and a detail page never disagree about the same tree.

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  deleteDatapack,
} from "@/lib/api/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { DATAPACKS_QUERY_KEY } from "@/features/datapacks/hooks/useDatapacks";
import {
  createCollection,
  createCollectionFolder,
  deleteCollection,
  deleteCollectionFolder,
  getCollection,
  listDatapackPaths,
  listCollections,
  listCollectionThemes,
  placeDatapack,
  renameCollectionFolder,
  unplaceDatapack,
  updateCollection,
  type CreateCollectionBody,
  type DatapackPathMap,
  type CollectionLists,
  type CollectionDetail,
  type CollectionTheme,
} from "@/lib/api/collectionsApi";

/** Query keys. Exported so any surface can invalidate the same entries —
 *  a mutation elsewhere must hit these exact keys or the UI goes stale. */
export const COLLECTIONS_QUERY_KEY = "collections";
export const COLLECTION_THEMES_QUERY_KEY = "collection-themes";
export const DATAPACK_PATHS_QUERY_KEY = "datapack-paths";

/** Matches useDatapacks: long enough that navigating between surfaces inside
 *  a minute doesn't refetch, short enough that a new collection appears on the
 *  next nav even if an invalidation were missed. */
const COLLECTIONS_STALE_TIME_MS = 30_000;

const EMPTY_LISTS: CollectionLists = { owned: [], shared: [] };

/** Both lists in one call. Callers that only want one destructure it. */
export function useCollections(options?: { enabled?: boolean }) {
  return useQuery<CollectionLists>({
    queryKey: [COLLECTIONS_QUERY_KEY],
    queryFn: () => listCollections(),
    staleTime: COLLECTIONS_STALE_TIME_MS,
    enabled: options?.enabled ?? true,
    placeholderData: EMPTY_LISTS,
  });
}

export function useCollection(collectionId: string | undefined) {
  return useQuery<CollectionDetail>({
    queryKey: [COLLECTIONS_QUERY_KEY, collectionId],
    queryFn: () => getCollection(collectionId as string),
    staleTime: COLLECTIONS_STALE_TIME_MS,
    enabled: Boolean(collectionId),
  });
}

/** The theme registry is a compile-time constant on the server, so it is
 *  fetched once and never refetched for the session. */
export function useCollectionThemes(options?: { enabled?: boolean }) {
  return useQuery<CollectionTheme[]>({
    queryKey: [COLLECTION_THEMES_QUERY_KEY],
    queryFn: () => listCollectionThemes(),
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
    staleTime: COLLECTIONS_STALE_TIME_MS,
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
    void queryClient.invalidateQueries({ queryKey: [COLLECTIONS_QUERY_KEY] });
    void queryClient.invalidateQueries({ queryKey: [DATAPACK_PATHS_QUERY_KEY] });
  };
}

export function useCreateCollection() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: CreateCollectionBody) => createCollection(body),
    onSuccess: invalidate,
  });
}

export function useUpdateCollection(collectionId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: { collection_name: string; description?: string }) =>
      updateCollection(collectionId, body),
    onSuccess: invalidate,
  });
}

export function useDeleteCollection() {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (collectionId: string) => deleteCollection(collectionId),
    onSuccess: invalidate,
  });
}

export function useCreateFolder(collectionId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (body: { folder_name: string; parent_folder_id?: string }) =>
      createCollectionFolder(collectionId, body),
    onSuccess: invalidate,
  });
}

export function useRenameFolder(collectionId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (args: { folderId: string; folderName: string }) =>
      renameCollectionFolder(collectionId, args.folderId, { folder_name: args.folderName }),
    onSuccess: invalidate,
  });
}

export function useDeleteFolder(collectionId: string) {
  const invalidate = useInvalidateTree();
  return useMutation({
    mutationFn: (folderId: string) => deleteCollectionFolder(collectionId, folderId),
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

/** Deletes a datapack from inside a collection, then refreshes everything that
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
