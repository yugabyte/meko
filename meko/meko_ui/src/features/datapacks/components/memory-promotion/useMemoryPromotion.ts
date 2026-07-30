// Loads paginated memories for a datapack, selection, and POST /promote-memories (promote to KB).
// Uses local page/pageSize for Prev/Next navigation in the learnings summary route.
//
// After a successful promote we refetch from the server (which now omits the
// just-promoted rows) and invoke `onPromoted(count)` so the parent can refresh
// downstream views (e.g. CollectiveLearningPipeline learnings count) and show
// a toast.

import { useCallback, useEffect, useState } from "react";
import {
  getApiErrorMessage,
  getCurrentUser,
  keepAsMemory,
  listMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
  promoteMemories,
  type Memory,
  type PromoteMemoriesResult,
} from "@/lib/api/mekoApi";

export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
export const DEFAULT_PAGE_SIZE = 10;

type PromotionSummary = {
  attempted: number;
  inserted: number;
  updated: number;
  notFound: number;
};

export function useMemoryPromotion({
  datapackId,
  onPromoted,
  q = "",
}: {
  datapackId: string;
  /** Optional substring filter forwarded to GET /memories?q=…. */
  q?: string;
  /** Fired after a successful promote with the count of just-promoted memories
   *  (inserted + updated). Parent typically refetches the datapack to refresh
   *  collective_memory_count and shows a toast. */
  onPromoted?: (count: number) => void;
}) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memories, setMemories] = useState<Memory[]>([]);
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  /** False until `listMemories` finishes for the current datapack+account (avoids flashing "0 memories" before fetch). */
  const [memoriesSummaryReady, setMemoriesSummaryReady] = useState(false);

  const [accountId, setAccountId] = useState<string>("");
  const [accountLoading, setAccountLoading] = useState(true);
  const [accountError, setAccountError] = useState<string | null>(null);

  const [promoting, setPromoting] = useState(false);
  /** When promoting a single row, that memory id (toolbar bulk promote leaves this null). */
  const [promotingId, setPromotingId] = useState<string | null>(null);
  const [promoteError, setPromoteError] = useState<string | null>(null);
  const [lastOutcome, setLastOutcome] = useState<PromotionSummary | null>(null);

  useEffect(() => {
    const controller = new AbortController();
    setAccountLoading(true);
    setAccountError(null);
    getCurrentUser({ signal: controller.signal })
      .then((user) => {
        if (controller.signal.aborted) return;
        setAccountId(user.account_id);
      })
      .catch((err) => {
        if (controller.signal.aborted) return;
        setAccountError(getApiErrorMessage(err, "Failed to load current user."));
      })
      .finally(() => {
        if (controller.signal.aborted) return;
        setAccountLoading(false);
      });
    return () => controller.abort();
  }, []);

  const loadMemories = useCallback(async () => {
    if (!accountId) return;
    setLoading(true);
    setError(null);
    setLastOutcome(null);
    setPromoteError(null);
    try {
      const resp = await listMemories({
        datapack_id: datapackId,
        // user_id: accountId,
        user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
        page,
        page_size: pageSize,
        q: q.trim() || undefined,
        scope: "learnings",
      });
      setMemories(resp.memories ?? []);
      setSelected(new Set());
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      if (typeof resp.page === "number" && resp.page > 0 && resp.page !== page) {
        setPage(resp.page);
      }
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to load memories."));
      setMemories([]);
      setTotal(0);
      setTotalPages(1);
    } finally {
      setLoading(false);
      setMemoriesSummaryReady(true);
    }
  }, [accountId, datapackId, page, pageSize, q]);

  useEffect(() => {
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    setMemoriesSummaryReady(false);
  }, [datapackId, accountId]);

  // Reset to page 1 when the search filter changes.
  useEffect(() => {
    setPage(1);
  }, [q]);

  useEffect(() => {
    if (!datapackId || !accountId) return;
    void loadMemories();
  }, [datapackId, accountId, loadMemories]);

  const goToPage = useCallback(
    (next: number) => {
      const clamped = Math.min(Math.max(1, next), totalPages);
      if (clamped === page) return;
      setPage(clamped);
    },
    [page, totalPages],
  );

  const changePageSize = useCallback((next: number) => {
    setPageSize(next);
    setPage(1);
  }, []);

  const rangeStart = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const rangeEnd = total === 0 ? 0 : Math.min(total, page * pageSize);

  const toggleOne = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const unpromotedMemories = memories;
  const selectableIds = unpromotedMemories.map((m) => m.id);
  const allChecked = selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));
  const someChecked = selected.size > 0 && !allChecked;

  const toggleAll = useCallback(
    (_checked?: boolean | "indeterminate") => {
      if (allChecked) {
        setSelected(new Set());
      } else {
        setSelected(new Set(selectableIds));
      }
    },
    [allChecked, selectableIds],
  );

  const promoteMemoryIds = useCallback(
    async (memoryIds: string[]) => {
      if (memoryIds.length === 0 || !accountId) return;

      setPromotingId(memoryIds.length === 1 ? memoryIds[0]! : null);
      setPromoting(true);
      setPromoteError(null);
      setLastOutcome(null);

      // Optimistic UI: drop the just-clicked row(s) immediately so the tile
      // vanishes without waiting for the BE round trip. We snapshot ONLY the
      // rows being promoted (with their original indices) plus the
      // optimistically-decremented total — not the full arrays — so a
      // rollback merges the rows back into whatever state exists at that
      // moment instead of clobbering concurrent updates the user made during
      // the in-flight call (manual refresh, checkbox toggle on another row,
      // pagination, etc.).
      const idsBeingPromoted = new Set(memoryIds);
      const removedRows: { index: number; memory: Memory }[] = [];
      memories.forEach((m, i) => {
        if (idsBeingPromoted.has(m.id)) {
          removedRows.push({ index: i, memory: m });
        }
      });
      const removedSelectedIds = memoryIds.filter((id) => selected.has(id));
      setMemories((prev) => prev.filter((m) => !idsBeingPromoted.has(m.id)));
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of memoryIds) next.delete(id);
        return next;
      });
      // Keep the header count in sync with the list (otherwise we'd render
      // "10 memories" with fewer rows visible). The successful path's
      // loadMemories() reconciles this from the server.
      setTotal((prev) => Math.max(0, prev - removedRows.length));

      try {
        const result: PromoteMemoriesResult = await promoteMemories({
          user_id: accountId,
          datapack_id: datapackId,
          memory_ids: memoryIds,
        });
        const promotedCount = result.inserted_ids.length + result.updated_ids.length;
        setLastOutcome({
          attempted: memoryIds.length,
          inserted: result.inserted_ids.length,
          updated: result.updated_ids.length,
          notFound: result.not_found_ids.length,
        });
        // Authoritative refetch: brings in rows that should now fill the gap
        // (preview window) and reconciles any not_found / partial-success
        // semantics + total with the server.
        await loadMemories();
        if (promotedCount > 0) {
          onPromoted?.(promotedCount);
        }
      } catch (err) {
        // Roll back by merging the removed rows + selection ids + total back
        // into the *current* state (don't replace it), so anything the user
        // changed during the in-flight call survives.
        setMemories((current) => {
          const next = [...current];
          // Insert in ascending original-index order so positions are stable.
          const ordered = [...removedRows].sort((a, b) => a.index - b.index);
          for (const { index, memory } of ordered) {
            // Skip if a concurrent refresh already brought the row back.
            if (next.some((m) => m.id === memory.id)) continue;
            const insertAt = Math.min(index, next.length);
            next.splice(insertAt, 0, memory);
          }
          return next;
        });
        setSelected((current) => {
          if (removedSelectedIds.length === 0) return current;
          const next = new Set(current);
          for (const id of removedSelectedIds) next.add(id);
          return next;
        });
        setTotal((prev) => prev + removedRows.length);
        setPromoteError(getApiErrorMessage(err, "Promotion failed."));
      } finally {
        setPromoting(false);
        setPromotingId(null);
      }
    },
    [accountId, loadMemories, memories, onPromoted, selected],
  );

  const promote = useCallback(async () => {
    const memoryIds = memories.filter((m) => selected.has(m.id)).map((m) => m.id);
    await promoteMemoryIds(memoryIds);
  }, [memories, selected, promoteMemoryIds]);

  const promoteOne = useCallback(
    (memoryId: string) => {
      void promoteMemoryIds([memoryId]);
    },
    [promoteMemoryIds],
  );

  // "Keep as memory" sibling of promote: stamps mem0 rows with
  // `keep_as_memory` so they drop from Learnings (this list) but stay
  // in Memories. Same snapshot-and-merge rollback pattern as
  // `promoteMemoryIds` — see the long comment there for the rationale
  // (snapshot the removed rows + their original indices + the affected
  // selection ids, then merge them back into *current* state on failure
  // so any concurrent edit during the in-flight call survives).
  const keepMemoryIds = useCallback(
    async (memoryIds: string[]) => {
      if (memoryIds.length === 0) return;
      const ids = new Set(memoryIds);
      const removedRows: { index: number; memory: Memory }[] = [];
      memories.forEach((m, i) => {
        if (ids.has(m.id)) {
          removedRows.push({ index: i, memory: m });
        }
      });
      const removedSelectedIds = memoryIds.filter((id) => selected.has(id));
      setMemories((prev) => prev.filter((m) => !ids.has(m.id)));
      setSelected((prev) => {
        const next = new Set(prev);
        for (const id of memoryIds) next.delete(id);
        return next;
      });
      setTotal((prev) => Math.max(0, prev - removedRows.length));
      try {
        await keepAsMemory({ datapack_id: datapackId, memory_ids: memoryIds });
        await loadMemories();
      } catch (err) {
        // Roll back into *current* state so a concurrent change (manual
        // refresh, checkbox toggle, pagination) isn't clobbered.
        setMemories((current) => {
          const next = [...current];
          const ordered = [...removedRows].sort((a, b) => a.index - b.index);
          for (const { index, memory } of ordered) {
            if (next.some((m) => m.id === memory.id)) continue;
            const insertAt = Math.min(index, next.length);
            next.splice(insertAt, 0, memory);
          }
          return next;
        });
        setSelected((current) => {
          if (removedSelectedIds.length === 0) return current;
          const next = new Set(current);
          for (const id of removedSelectedIds) next.add(id);
          return next;
        });
        setTotal((prev) => prev + removedRows.length);
        setError(getApiErrorMessage(err, "Failed to keep memories."));
        // Fire-and-forget reconcile so the server's view eventually
        // lands even if the rollback above is the immediate truth.
        void loadMemories();
      }
    },
    [datapackId, loadMemories, memories, selected],
  );

  const keep = useCallback(async () => {
    const memoryIds = memories.filter((m) => selected.has(m.id)).map((m) => m.id);
    await keepMemoryIds(memoryIds);
  }, [memories, selected, keepMemoryIds]);

  const keepOne = useCallback(
    (memoryId: string) => {
      void keepMemoryIds([memoryId]);
    },
    [keepMemoryIds],
  );

  return {
    loading,
    memoriesSummaryReady,
    error,
    memories,
    selected,
    page,
    pageSize,
    total,
    totalPages,
    accountId,
    accountLoading,
    accountError,
    promoting,
    promotingId,
    promoteError,
    lastOutcome,
    unpromotedMemories,
    selectableIds,
    allChecked,
    someChecked,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
    toggleOne,
    toggleAll,
    promote,
    promoteOne,
    keep,
    keepOne,
  };
}
