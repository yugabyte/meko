// Read-only paginated list of promoted memories for a datapack
// (collective-memory-summary route).
//
// Mirrors useMemoryReadOnlyList but hits GET /promoted-memories instead of
// /memories — the promoted endpoint is datapack-scoped only (no user_id).

import { useCallback, useEffect, useState } from "react";
import {
  getApiErrorMessage,
  listPromotedMemories,
  type PromotedMemory,
} from "@/lib/api/mekoApi";
import { DEFAULT_PAGE_SIZE } from "@/features/datapacks/components/memory-promotion/useMemoryPromotion";

export function usePromotedMemoryList({
  datapackId,
}: {
  datapackId: string;
}) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memories, setMemories] = useState<PromotedMemory[]>([]);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);
  const [listReady, setListReady] = useState(false);

  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);

  // Reset pagination *during render* when the datapack changes so the fetch
  // effect below sees fresh values on the very first render — otherwise we
  // fire GET /promoted-memories twice on a datapack switch (once with the
  // stale page captured by `loadMemories`'s closure, then again after the
  // reset effect commits and `loadMemories` is rebuilt).
  // See: https://react.dev/reference/react/useState#storing-information-from-previous-renders
  const [prevDatapackId, setPrevDatapackId] = useState(datapackId);
  if (prevDatapackId !== datapackId) {
    setPrevDatapackId(datapackId);
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    setListReady(false);
  }

  const loadMemories = useCallback(async () => {
    if (!datapackId) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await listPromotedMemories({
        datapack_id: datapackId,
        page,
        page_size: pageSize,
      });
      setMemories(resp.promoted_memories ?? []);
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      if (typeof resp.page === "number" && resp.page > 0 && resp.page !== page) {
        setPage(resp.page);
      }
      setListReady(true);
    } catch (err) {
      setError(getApiErrorMessage(err, "Failed to load collective memories."));
      setMemories([]);
      setTotal(0);
      setTotalPages(1);
    } finally {
      setLoading(false);
    }
  }, [datapackId, page, pageSize]);

  useEffect(() => {
    if (!datapackId) return;
    void loadMemories();
  }, [datapackId, loadMemories]);

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

  return {
    loading,
    error,
    memories,
    total,
    totalPages,
    page,
    pageSize,
    listReady,
    rangeStart,
    rangeEnd,
    loadMemories,
    goToPage,
    changePageSize,
  };
}
