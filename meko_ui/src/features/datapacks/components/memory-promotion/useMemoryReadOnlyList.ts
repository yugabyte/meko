/**
 * Read-only memory list: same GET /memories contract as promotion flow, without selection or promote.
 * `preview` = first {@link MEMORY_READ_ONLY_PREVIEW_SIZE} rows (detail Memory tab); `full` = paginated (memory-summary route).
 */
import { useCallback, useEffect, useState } from "react";
import {
  getApiErrorMessage,
  getCurrentUser,
  listMemories,
  MEMORY_LIST_DEFAULT_USER_TYPE,
  type Memory,
} from "@/lib/mekoApi";
import { DEFAULT_PAGE_SIZE } from "./useMemoryPromotion";

export const MEMORY_READ_ONLY_PREVIEW_SIZE = 10;

export type MemoryReadOnlyVariant = "preview" | "full";

export function useMemoryReadOnlyList({
  datapackId,
  variant,
}: {
  datapackId: string;
  variant: MemoryReadOnlyVariant;
}) {
  const isFull = variant === "full";

  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memories, setMemories] = useState<Memory[]>([]);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [listReady, setListReady] = useState(false);

  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);

  const [accountId, setAccountId] = useState("");
  const [accountLoading, setAccountLoading] = useState(true);
  const [accountError, setAccountError] = useState<string | null>(null);

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

  const effectivePage = isFull ? page : 1;
  const effectivePageSize = isFull ? pageSize : MEMORY_READ_ONLY_PREVIEW_SIZE;

  const loadMemories = useCallback(async () => {
    if (!accountId) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await listMemories({
        datapack_id: datapackId,
        user_id: accountId,
        user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
        page: effectivePage,
        page_size: effectivePageSize,
      });
      setMemories(resp.memories ?? []);
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      if (isFull && typeof resp.page === "number" && resp.page > 0 && resp.page !== effectivePage) {
        setPage(resp.page);
      }
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to load memories."));
      setMemories([]);
      setTotal(0);
      setTotalPages(1);
    } finally {
      setLoading(false);
      setListReady(true);
    }
  }, [accountId, datapackId, effectivePage, effectivePageSize, isFull]);

  useEffect(() => {
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    setListReady(false);
  }, [datapackId, accountId]);

  useEffect(() => {
    if (!datapackId || !accountId) return;
    void loadMemories();
  }, [datapackId, accountId, loadMemories]);

  const goToPage = useCallback(
    (next: number) => {
      if (!isFull) return;
      const clamped = Math.min(Math.max(1, next), totalPages);
      if (clamped === page) return;
      setPage(clamped);
    },
    [isFull, page, totalPages],
  );

  const changePageSize = useCallback(
    (next: number) => {
      if (!isFull) return;
      setPageSize(next);
      setPage(1);
    },
    [isFull],
  );

  const rangeStart = total === 0 ? 0 : (effectivePage - 1) * effectivePageSize + 1;
  const rangeEnd = total === 0 ? 0 : Math.min(total, effectivePage * effectivePageSize);

  const sessionSummaryReady =
    !accountLoading && accountError == null && Boolean(accountId) && listReady;

  return {
    isFull,
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    accountLoading,
    accountError,
    sessionSummaryReady,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
  };
}
