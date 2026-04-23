import { useCallback, useEffect, useMemo, useState } from "react";
import {
  BrainCircuit,
  Loader2,
  AlertCircle,
  CheckCircle2,
  ArrowUpToLine,
  RefreshCw,
  ChevronLeft,
  ChevronRight,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { cn } from "@/lib/utils";
import {
  getCurrentUser,
  listMemories,
  syncMemoriesToKB,
  type Memory,
  type TenantKBSyncResult,
} from "@/lib/mekoApi";

type Props = {
  datapackName: string;
  datapackId: string;
};

type PromoteOutcome = {
  attempted: number;
  inserted: number;
  updated: number;
  notFound: number;
};

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
const DEFAULT_PAGE_SIZE = 10;

// localStorage caches the full Memory objects the user has already promoted
// to KB, scoped per (accountId, datapackId). We cache the whole row (not
// just the id) so the "Already promoted" section can render on every page
// of the paginated mem0 list, not only the page the row originally came
// from. Used for UI dimming/sorting only — the backend is the source of
// truth (it UPDATEs instead of INSERTing on re-promote), but there's no
// "undo" API yet, so we make it hard to re-promote by accident.
const PROMOTED_STORAGE_PREFIX = "meko:promoted-memories:v2";

type PromotedEntry = {
  memory: Memory;
  promotedAt: number;
};

function promotedStorageKey(accountId: string, datapackId: string): string {
  return `${PROMOTED_STORAGE_PREFIX}:${accountId}:${datapackId}`;
}

function readPromotedCache(
  accountId: string,
  datapackId: string,
): Map<string, PromotedEntry> {
  if (!accountId || !datapackId) return new Map();
  try {
    const raw = localStorage.getItem(promotedStorageKey(accountId, datapackId));
    if (!raw) return new Map();
    const parsed = JSON.parse(raw);
    if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
      const out = new Map<string, PromotedEntry>();
      for (const [id, entry] of Object.entries(
        parsed as Record<string, unknown>,
      )) {
        if (
          entry &&
          typeof entry === "object" &&
          "memory" in entry &&
          typeof (entry as { memory?: { id?: unknown } }).memory?.id === "string"
        ) {
          const e = entry as PromotedEntry;
          out.set(id, {
            memory: e.memory,
            promotedAt:
              typeof e.promotedAt === "number" ? e.promotedAt : Date.now(),
          });
        }
      }
      return out;
    }
  } catch {
    // Corrupt payload — drop it so we start clean.
    localStorage.removeItem(promotedStorageKey(accountId, datapackId));
  }
  return new Map();
}

function writePromotedCache(
  accountId: string,
  datapackId: string,
  cache: Map<string, PromotedEntry>,
) {
  if (!accountId || !datapackId) return;
  const obj: Record<string, PromotedEntry> = {};
  cache.forEach((entry, id) => {
    obj[id] = entry;
  });
  localStorage.setItem(
    promotedStorageKey(accountId, datapackId),
    JSON.stringify(obj),
  );
}

export default function MemoriesPanel({ datapackName, datapackId }: Props) {
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [memories, setMemories] = useState<Memory[]>([]);
  // Selection is per-page: switching pages / page size / datapack clears it.
  const [selected, setSelected] = useState<Set<string>>(new Set());

  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);
  const [total, setTotal] = useState(0);
  const [totalPages, setTotalPages] = useState(1);

  // Logged-in Meko account_id. Both GET /memories and POST /tenant-kb/sync
  // expect this as `user_id`: mem0 rows are filtered on
  // `payload->>'user_id' = account_id`, and the KB sync writes it into
  // knowledge_base.user_id.
  const [accountId, setAccountId] = useState<string>("");
  const [accountLoading, setAccountLoading] = useState(true);
  const [accountError, setAccountError] = useState<string | null>(null);

  const [promoting, setPromoting] = useState(false);
  const [promoteError, setPromoteError] = useState<string | null>(null);
  const [lastOutcome, setLastOutcome] = useState<PromoteOutcome | null>(null);

  // Full Memory objects the user has already promoted to KB from this
  // (account, datapack) pair. Rehydrated from localStorage once both ids
  // are known and on every datapack switch. Stored as a Map so the
  // "Already promoted" section renders across every page of the mem0 list,
  // not just the page the row originally came from.
  const [promoted, setPromoted] = useState<Map<string, PromotedEntry>>(
    () => new Map(),
  );

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
        setAccountError(
          err instanceof Error ? err.message : "Failed to load current user",
        );
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
        user_id: accountId,
        datapack_id: datapackId,
        free_user: true,
        page,
        page_size: pageSize,
      });
      setMemories(resp.memories ?? []);
      setSelected(new Set());
      setTotal(resp.total ?? 0);
      setTotalPages(Math.max(1, resp.total_pages ?? 1));
      // Server may clamp page on last-page deletions — align local state.
      if (typeof resp.page === "number" && resp.page > 0 && resp.page !== page) {
        setPage(resp.page);
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load memories");
      setMemories([]);
      setTotal(0);
      setTotalPages(1);
    } finally {
      setLoading(false);
    }
  }, [accountId, datapackId, page, pageSize]);

  // Reset to page 1 whenever the datapack changes so we don't land on a
  // page that doesn't exist in the new datapack's result set.
  useEffect(() => {
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
  }, [datapackId]);

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

  const toggleOne = (id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  // Promoted memories are sorted to the bottom, dimmed, and disabled — so
  // "select all" and the selection counter operate only on unpromoted rows.
  // Unpromoted is scoped to the current page (server-paginated); promoted
  // is sourced from the localStorage cache so it persists across page
  // changes.
  const unpromotedMemories = useMemo(
    () => memories.filter((m) => !promoted.has(m.id)),
    [memories, promoted],
  );
  const promotedMemories = useMemo(
    () =>
      Array.from(promoted.values())
        .sort((a, b) => b.promotedAt - a.promotedAt)
        .map((e) => e.memory),
    [promoted],
  );
  const selectableIds = useMemo(
    () => unpromotedMemories.map((m) => m.id),
    [unpromotedMemories],
  );
  const allChecked =
    selectableIds.length > 0 && selectableIds.every((id) => selected.has(id));
  const someChecked = selected.size > 0 && !allChecked;

  const toggleAll = () => {
    if (allChecked) {
      setSelected(new Set());
    } else {
      setSelected(new Set(selectableIds));
    }
  };

  const promote = useCallback(async () => {
    if (selected.size === 0 || !accountId) return;

    // Every mem0 row surfaced by /memories for this panel already matches
    // `payload.user_id == accountId`, so a single /tenant-kb/sync call
    // with user_id = accountId covers all selected ids.
    const memoryIds = memories
      .filter((m) => selected.has(m.id))
      .map((m) => m.id);

    setPromoting(true);
    setPromoteError(null);
    setLastOutcome(null);

    try {
      const result: TenantKBSyncResult = await syncMemoriesToKB({
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
      // Both inserted and updated rows now live in knowledge_base — mark
      // them promoted locally so the UI dims/disables them on next render.
      // not_found_ids are intentionally NOT marked (they weren't promoted).
      const justPromoted = [
        ...(result.inserted_ids ?? []),
        ...(result.updated_ids ?? []),
      ];
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
      setSelected(new Set());
    } catch (err) {
      setPromoteError(err instanceof Error ? err.message : "Promotion failed");
    } finally {
      setPromoting(false);
    }
  }, [accountId, datapackId, memories, selected]);

  return (
    <div className="rounded-xl border border-border bg-[#1C2333] p-6 space-y-4">
      <div className="flex items-center justify-between gap-4">
        <div className="flex items-center gap-4">
          <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-purple-500/10">
            <BrainCircuit className="w-5 h-5 text-purple-400" />
          </div>
          <div>
            <h3 className="text-sm font-medium text-white">Agent Memories</h3>
            <p className="text-xs text-muted-foreground mt-0.5">
              Memories stored by agents in{" "}
              <span className="text-white">{datapackName}</span>. Select any to
              promote them into the knowledge base.
            </p>
          </div>
        </div>
        <Button
          variant="outline"
          className="gap-2 border-border bg-background text-muted-foreground hover:bg-muted hover:text-foreground"
          onClick={loadMemories}
          disabled={loading || accountLoading || !accountId}
        >
          {loading || accountLoading ? (
            <Loader2 className="w-4 h-4 animate-spin" />
          ) : (
            <RefreshCw className="w-4 h-4" />
          )}
          {loading || accountLoading ? "Loading..." : "Refresh"}
        </Button>
      </div>

      {accountError && (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
          <AlertCircle className="w-4 h-4 shrink-0" />
          {accountError}
        </div>
      )}

      {error && (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
          <AlertCircle className="w-4 h-4 shrink-0" />
          {error}
        </div>
      )}

      {memories.length > 0 && (
        <div className="flex items-center justify-between rounded-lg border border-border bg-[#0E1525] px-4 py-2.5">
          <label className="flex items-center gap-2 cursor-pointer">
            <Checkbox
              checked={
                allChecked ? true : someChecked ? "indeterminate" : false
              }
              onCheckedChange={toggleAll}
              disabled={selectableIds.length === 0}
            />
            <span className="text-xs text-muted-foreground">
              {selected.size > 0
                ? `${selected.size} of ${selectableIds.length} selectable on this page`
                : selectableIds.length === 0
                  ? `Nothing to promote on this page (${promotedMemories.length} already promoted)`
                  : `Select all selectable on this page (${selectableIds.length}${total > memories.length ? ` of ${total}` : ""})`}
            </span>
          </label>
          <Button
            className="gap-2 bg-purple-600 hover:bg-purple-700 text-white"
            onClick={promote}
            disabled={promoting || selected.size === 0}
          >
            {promoting ? (
              <Loader2 className="w-4 h-4 animate-spin" />
            ) : (
              <ArrowUpToLine className="w-4 h-4" />
            )}
            {promoting
              ? "Promoting..."
              : `Promote ${selected.size || ""} to KB`.trim()}
          </Button>
        </div>
      )}

      {promoteError && (
        <div className="flex items-center gap-2 rounded-lg border border-red-500/30 bg-red-500/10 px-4 py-2 text-xs text-red-300">
          <AlertCircle className="w-4 h-4 shrink-0" />
          {promoteError}
        </div>
      )}
      {lastOutcome && (
        <div className="flex items-center gap-2 rounded-lg border border-green-500/30 bg-green-500/10 px-4 py-2 text-xs text-green-300">
          <CheckCircle2 className="w-4 h-4 shrink-0" />
          Promoted {lastOutcome.attempted} selected · {lastOutcome.inserted}{" "}
          inserted · {lastOutcome.updated} updated
          {lastOutcome.notFound > 0 && (
            <> · {lastOutcome.notFound} skipped</>
          )}
        </div>
      )}

      {memories.length === 0 && !loading && !error && (
        <p className="text-xs text-muted-foreground">
          No memories found for this datapack yet.
        </p>
      )}

      {total > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border bg-[#0E1525] px-4 py-2.5 text-xs text-muted-foreground">
          <div>
            {total === 0
              ? "0 memories"
              : `Showing ${rangeStart}–${rangeEnd} of ${total}`}
          </div>
          <div className="flex items-center gap-3">
            <label className="flex items-center gap-2">
              <span>Per page</span>
              <select
                value={pageSize}
                onChange={(e) => changePageSize(Number(e.target.value))}
                disabled={loading}
                className="rounded-md border border-border bg-background px-2 py-1 text-xs text-foreground focus:outline-none focus:ring-1 focus:ring-ring disabled:opacity-50"
              >
                {PAGE_SIZE_OPTIONS.map((opt) => (
                  <option key={opt} value={opt}>
                    {opt}
                  </option>
                ))}
              </select>
            </label>
            <div className="flex items-center gap-1">
              <Button
                variant="outline"
                size="sm"
                className="gap-1 px-2"
                onClick={() => goToPage(page - 1)}
                disabled={loading || page <= 1}
                aria-label="Previous page"
              >
                <ChevronLeft className="w-4 h-4" />
                Prev
              </Button>
              <span className="px-2 text-muted-foreground">
                Page <span className="text-white">{page}</span> of {totalPages}
              </span>
              <Button
                variant="outline"
                size="sm"
                className="gap-1 px-2"
                onClick={() => goToPage(page + 1)}
                disabled={loading || page >= totalPages}
                aria-label="Next page"
              >
                Next
                <ChevronRight className="w-4 h-4" />
              </Button>
            </div>
          </div>
        </div>
      )}

      {(memories.length > 0 || promotedMemories.length > 0) && (
        <ul className="space-y-2">
          {/* Unpromoted first: normal styling, enabled checkbox. */}
          {unpromotedMemories.map((m) => {
            const checked = selected.has(m.id);
            const data = m.payload?.data ?? "";
            const agent = m.payload?.agent_id ?? "";
            return (
              <li
                key={m.id}
                className={cn(
                  "rounded-lg border bg-[#0E1525] px-4 py-3 flex items-start gap-3",
                  checked
                    ? "border-purple-500/50 bg-purple-500/5"
                    : "border-border",
                )}
              >
                <Checkbox
                  checked={checked}
                  onCheckedChange={() => toggleOne(m.id)}
                  className="mt-0.5"
                />
                <div className="min-w-0 flex-1 space-y-1">
                  <p className="text-sm text-white break-words">
                    {data || (
                      <span className="italic text-muted-foreground">
                        (empty payload.data)
                      </span>
                    )}
                  </p>
                  {agent && (
                    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
                      <span>
                        agent: <code className="text-blue-300">{agent}</code>
                      </span>
                    </div>
                  )}
                </div>
              </li>
            );
          })}

          {/* Divider between live rows (current page) and already-promoted
              rows (cached across all pages). */}
          {promotedMemories.length > 0 && (
            <li
              className="pt-2 pb-1 text-[10px] uppercase tracking-wider text-muted-foreground/70"
              aria-hidden
            >
              Already promoted · {promotedMemories.length}
            </li>
          )}

          {/* Promoted at the bottom: dimmed, disabled, badge. No undo — the
              backend has no delete-from-KB API yet. */}
          {promotedMemories.map((m) => {
            const data = m.payload?.data ?? "";
            const agent = m.payload?.agent_id ?? "";
            return (
              <li
                key={m.id}
                className="rounded-lg border border-dashed border-border/60 bg-[#0E1525]/40 px-4 py-3 flex items-start gap-3 opacity-50"
                aria-label="Already promoted to knowledge base"
              >
                <Checkbox
                  checked={false}
                  disabled
                  className="mt-0.5 cursor-not-allowed"
                />
                <div className="min-w-0 flex-1 space-y-1">
                  <p className="text-sm text-white break-words line-through decoration-muted-foreground/40">
                    {data || (
                      <span className="italic text-muted-foreground">
                        (empty payload.data)
                      </span>
                    )}
                  </p>
                  <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
                    <span className="inline-flex items-center gap-1 rounded-full border border-green-500/30 bg-green-500/10 px-2 py-0.5 text-[10px] font-medium text-green-300">
                      <CheckCircle2 className="w-3 h-3" />
                      Promoted to KB
                    </span>
                    {agent && (
                      <span>
                        agent: <code className="text-blue-300">{agent}</code>
                      </span>
                    )}
                  </div>
                </div>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
