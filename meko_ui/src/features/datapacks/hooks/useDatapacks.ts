// useDatapacks — react-query wrapper around GET /datapacks.
//
// The signed-in user's datapack list is referenced from several places
// (sidebar, overview tiles, the Connect page's MCP-URL lookup, etc.).
// Without a shared cache each surface would refetch on mount and we'd
// hit `/datapacks` 3-5 times on first paint. This hook is the single
// source of truth: every caller shares the same cache entry.
//
// `staleTime: 30s` is a deliberate middle ground — long enough that
// route-changes inside one minute don't refetch, short enough that a
// freshly-created datapack shows up on the next nav without manual
// invalidation. Mutations that change the list (create/delete/rename)
// should still call `queryClient.invalidateQueries({ queryKey:
// [DATAPACKS_QUERY_KEY] })` for instant feedback rather than waiting
// out the staleness window.

import { useQuery } from "@tanstack/react-query";
import { listDatapacks, type Datapack } from "@/lib/mekoApi";

export const DATAPACKS_QUERY_KEY = "datapacks";

export function useDatapacks(options?: { enabled?: boolean }) {
  return useQuery<Datapack[]>({
    queryKey: [DATAPACKS_QUERY_KEY],
    queryFn: () => listDatapacks(),
    staleTime: 30_000,
    enabled: options?.enabled ?? true,
  });
}
