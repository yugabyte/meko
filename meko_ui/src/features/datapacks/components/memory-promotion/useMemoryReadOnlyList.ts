/**
 * Read-only memory list: same GET /memories contract as promotion flow,
 * without selection or promote. Drives the paginated memory-summary route.
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

export function useMemoryReadOnlyList({
  datapackId,
  q = "",
}: {
  datapackId: string;
  /** Optional substring filter forwarded to GET /memories?q=…. */
  q?: string;
}) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memories, setMemories] = useState<Memory[]>([]);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [listReady, setListReady] = useState(false);
  const [langfuseProjectId, setLangfuseProjectId] = useState("");

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

  const loadMemories = useCallback(async () => {
    if (!accountId) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await listMemories({
        datapack_id: datapackId,
        user_id: accountId,
        user_type: MEMORY_LIST_DEFAULT_USER_TYPE,
        page,
        page_size: pageSize,
        q: q.trim() || undefined,
        scope: "memories",
      });
      setMemories(resp.memories ?? []);
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      setLangfuseProjectId(resp.langfuse_project_id ?? "");
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
      setListReady(true);
    }
  }, [accountId, datapackId, page, pageSize, q]);

  useEffect(() => {
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    setListReady(false);
  }, [datapackId, accountId]);

  // Reset to page 1 whenever the search filter changes so a new query
  // doesn't land on an out-of-range page number.
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

  const sessionSummaryReady =
    !accountLoading && accountError == null && Boolean(accountId) && listReady;

  return {
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    accountId,
    accountLoading,
    accountError,
    sessionSummaryReady,
    rangeStart,
    rangeEnd,
    langfuseProjectId,
    loadMemories,
    goToPage,
    changePageSize,
  };
}
