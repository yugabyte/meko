// Loads paginated memories for a datapack, selection, and POST /promote-memories (promote to KB).
// `preview` uses page 1 with a larger fetch window but only shows 3 unpromoted rows so promoting one
// still fills the preview; `full` uses local page/pageSize and Prev/Next navigation.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  getApiErrorMessage,
  getCurrentUser,
  listMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
  promoteMemories,
  type Memory,
  type PromoteMemoriesResult,
} from "@/lib/mekoApi";
import { readPromotedCache, writePromotedCache, type PromotedMemory } from "./promotedStorage";

export const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
export const DEFAULT_PAGE_SIZE = 10;
/** How many unpromoted rows the preview UI shows at once. */
export const PREVIEW_PAGE_SIZE = 3;
/** Preview fetches this many rows from page 1 so the next memories can fill in after a promotion. */
export const PREVIEW_FETCH_PAGE_SIZE = 30;

export type MemoryPromotionVariant = "full" | "preview";

type PromotionSummary = {
  attempted: number;
  inserted: number;
  updated: number;
  notFound: number;
};

export function useMemoryPromotion({
  datapackId,
  variant,
}: {
  datapackId: string;
  variant: MemoryPromotionVariant;
}) {
  const isPreview = variant === "preview";

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

  const [promoted, setPromoted] = useState<Map<string, PromotedMemory>>(() => new Map());

  useEffect(() => {
    if (!accountId || !datapackId) return;
    setPromoted(readPromotedCache(accountId, datapackId));
  }, [accountId, datapackId]);

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

  const effectivePage = isPreview ? 1 : page;
  const effectivePageSize = isPreview ? PREVIEW_FETCH_PAGE_SIZE : pageSize;

  const loadMemories = useCallback(async () => {
    if (!accountId) return;
    setLoading(true);
    setError(null);
    setLastOutcome(null);
    setPromoteError(null);
    try {
      const resp = await listMemories({
        datapack_id: datapackId,
        user_id: accountId,
        user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
        page: effectivePage,
        page_size: effectivePageSize,
      });
      setMemories(resp.memories ?? []);
      setSelected(new Set());
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      if (!isPreview && typeof resp.page === "number" && resp.page > 0 && resp.page !== effectivePage) {
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
  }, [accountId, datapackId, effectivePage, effectivePageSize, isPreview]);

  useEffect(() => {
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    setMemoriesSummaryReady(false);
  }, [datapackId, accountId]);

  useEffect(() => {
    if (!datapackId || !accountId) return;
    void loadMemories();
  }, [datapackId, accountId, loadMemories]);

  const goToPage = useCallback(
    (next: number) => {
      if (isPreview) return;
      const clamped = Math.min(Math.max(1, next), totalPages);
      if (clamped === page) return;
      setPage(clamped);
    },
    [isPreview, page, totalPages],
  );

  const changePageSize = useCallback(
    (next: number) => {
      if (isPreview) return;
      setPageSize(next);
      setPage(1);
    },
    [isPreview],
  );

  const rangeStart = total === 0 ? 0 : (effectivePage - 1) * effectivePageSize + 1;
  const rangeEnd = total === 0 ? 0 : Math.min(total, effectivePage * effectivePageSize);

  const toggleOne = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const unpromotedFromFetch = useMemo(
    () => memories.filter((m) => !promoted.has(m.id)),
    [memories, promoted],
  );
  const unpromotedMemories = useMemo(
    () =>
      isPreview ? unpromotedFromFetch.slice(0, PREVIEW_PAGE_SIZE) : unpromotedFromFetch,
    [isPreview, unpromotedFromFetch],
  );
  const promotedMemories = useMemo(
    () =>
      Array.from(promoted.values())
        .sort((a, b) => b.promotedAt - a.promotedAt)
        .map((e) => e.memory),
    [promoted],
  );
  const selectableIds = useMemo(() => unpromotedMemories.map((m) => m.id), [unpromotedMemories]);
  const allChecked = selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));
  const someChecked = selected.size > 0 && !allChecked;

  const toggleAll = useCallback((_checked?: boolean | "indeterminate") => {
    if (allChecked) {
      setSelected(new Set());
    } else {
      setSelected(new Set(selectableIds));
    }
  }, [allChecked, selectableIds]);

  const promoteMemoryIds = useCallback(
    async (memoryIds: string[]) => {
      if (memoryIds.length === 0 || !accountId) return;

      setPromotingId(memoryIds.length === 1 ? memoryIds[0]! : null);
      setPromoting(true);
      setPromoteError(null);
      setLastOutcome(null);

      try {
        const result: PromoteMemoriesResult = await promoteMemories({
          user_id: accountId,
          datapack_id: datapackId,
          memory_ids: memoryIds,
        });
        setLastOutcome({
          attempted: memoryIds.length,
          inserted: result.inserted_ids.length,
          updated: result.updated_ids.length,
          notFound: result.not_found_ids.length,
        });
        const justPromoted = [...(result.inserted_ids ?? []), ...(result.updated_ids ?? [])];
        if (justPromoted.length > 0) {
          const justPromotedSet = new Set(justPromoted);
          const promotedMems = memories.filter((m) => justPromotedSet.has(m.id));
          const now = Date.now();
          setPromoted((prev) => {
            const next = new Map(prev);
            for (const mem of promotedMems) {
              next.set(mem.id, { memory: mem, promotedAt: now });
            }
            writePromotedCache(accountId, datapackId, next);
            return next;
          });
        }
        setSelected((prev) => {
          const next = new Set(prev);
          for (const id of justPromoted) {
            next.delete(id);
          }
          return next;
        });
      } catch (err) {
        setPromoteError(getApiErrorMessage(err, "Promotion failed."));
      } finally {
        setPromoting(false);
        setPromotingId(null);
      }
    },
    [accountId, datapackId, memories],
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

  return {
    isPreview,
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
    promotedMemories,
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
  };
}
