// Conversations tab content: paginated card-list of Langfuse sessions
// for the selected datapack. Visually identical to the Learnings panel
// (purple accent, same row look, same pagination bar) so the two tabs
// feel like siblings.
//
// Stays mounted across tab switches so the cached page isn't refetched
// when the user toggles tabs. Refresh button reloads the current page.
//
// Pagination is offset-based against:
//   GET /api/v1/datapacks/:name/conversations?limit=&offset=
// The total is supplied by the parent (it already fetched it as part of
// `dp.conversations_count` from getDatapack), so the panel doesn't need
// a second roundtrip just to render "Showing X–Y of Z".

import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { ExternalLink, Loader2, MessageSquare } from "lucide-react";
import {
  getApiErrorMessage,
  listDatapackConversations,
  type DatapackConversation,
} from "@/lib/mekoApi";
import { ROUTES } from "@/app/routes/paths";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import DatapackPanelShell from "@/features/datapacks/components/datapack-detail/DatapackPanelShell";
import DatapackPanelPagination from "@/features/datapacks/components/datapack-detail/DatapackPanelPagination";

const PAGE_SIZE_OPTIONS = [10, 20, 50, 100] as const;
const DEFAULT_PAGE_SIZE = 20;

type Props = {
  datapackName: string;
  /** Authoritative session count for this datapack, sourced from
   *  `dp.conversations_count` on the parent. Used purely for pagination
   *  ("Showing X–Y of Z"); the listing endpoint does not return it. */
  total: number;
  className?: string;
};

export default function ConversationsPanel({ datapackName, total, className }: Props) {
  const navigate = useNavigate();
  const [conversations, setConversations] = useState<DatapackConversation[]>([]);
  /** Langfuse project_id for this datapack. Comes from the conversations
   *  list response (server-side join through langfuse_project_mapping).
   *  Empty until the first successful fetch resolves with a mapping —
   *  in that case the per-row "Open in Observe" button is hidden because
   *  there is nowhere to deep-link to. */
  const [langfuseProjectId, setLangfuseProjectId] = useState<string>("");
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState<number>(DEFAULT_PAGE_SIZE);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  /** True once the first fetch resolves — drives count/empty-state copy. */
  const [hasFetched, setHasFetched] = useState(false);

  const totalPages = useMemo(
    () => Math.max(1, Math.ceil(total / pageSize)),
    [total, pageSize],
  );
  const rangeStart = total === 0 ? 0 : (page - 1) * pageSize + 1;
  const rangeEnd = Math.min(total, page * pageSize);

  const load = useCallback(
    async (nextPage: number, nextSize: number, signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const resp = await listDatapackConversations(
          datapackName,
          { limit: nextSize, offset: (nextPage - 1) * nextSize },
          { signal },
        );
        if (signal?.aborted) return;
        setConversations(resp.conversations ?? []);
        setLangfuseProjectId(resp.langfuse_project_id ?? "");
        setHasFetched(true);
      } catch (err) {
        if (signal?.aborted) return;
        setError(getApiErrorMessage(err, "Failed to load conversations."));
      } finally {
        if (!signal?.aborted) setLoading(false);
      }
    },
    [datapackName],
  );

  // Reload whenever the datapack changes (resets to page 1).
  useEffect(() => {
    const ac = new AbortController();
    setConversations([]);
    setLangfuseProjectId("");
    setHasFetched(false);
    setPage(1);
    setPageSize(DEFAULT_PAGE_SIZE);
    void load(1, DEFAULT_PAGE_SIZE, ac.signal);
    return () => ac.abort();
  }, [datapackName, load]);

  /**
   * Navigate to the embedded Observe page with `project` + `session`
   * query params. ObservePage reads those and points its iframe at
   * the Langfuse trace detail page for this conversation (Meko's MCP
   * server sets trace.id == session_id == conversation_id, so the
   * sessionId we pass IS the Langfuse trace id).
   *
   * The button is rendered only when langfuseProjectId is known.
   */
  const openConversationInObserve = useCallback(
    (sessionId: string) => {
      if (!langfuseProjectId || !sessionId) return;
      const qs = new URLSearchParams({
        project: langfuseProjectId,
        session: sessionId,
      });
      navigate(`${ROUTES.observeHub}?${qs.toString()}`);
    },
    [langfuseProjectId, navigate],
  );

  const goToPage = useCallback(
    (next: number) => {
      const clamped = Math.max(1, Math.min(totalPages, next));
      if (clamped === page) return;
      setPage(clamped);
      void load(clamped, pageSize);
    },
    [page, pageSize, totalPages, load],
  );

  const changePageSize = useCallback(
    (next: number) => {
      if (next === pageSize) return;
      setPageSize(next);
      setPage(1);
      void load(1, next);
    },
    [pageSize, load],
  );

  const refresh = useCallback(() => {
    void load(page, pageSize);
  }, [load, page, pageSize]);

  const showEmpty = !error && hasFetched && total === 0;
  const showInitialLoader = loading && conversations.length === 0 && !error;
  const showPagination = total > 0;

  return (
    <DatapackPanelShell
      className={className}
      icon={<MessageSquare className="h-5 w-5 text-white" strokeWidth={1.75} aria-hidden />}
      iconAccentClass="bg-purple-500/15 ring-purple-500/10"
      title={
        <>
          Conversations
          {hasFetched ? (
            <span className="font-normal text-muted-foreground">
              {" "}
              · {total} {total === 1 ? "session" : "sessions"}
            </span>
          ) : (
            <span className="font-normal text-muted-foreground"> · </span>
          )}
        </>
      }
      description={
        <>
          Latest Langfuse sessions for{" "}
          <span className="font-medium text-white">{datapackName}</span>. Each row is one chat
          conversation.
        </>
      }
      onRefresh={refresh}
      refreshLoading={loading}
      errors={[error]}
    >
      {showEmpty ? (
        <p className="text-xs text-muted-foreground">
          No conversations recorded for this datapack yet.
        </p>
      ) : null}

      {showPagination ? (
        <DatapackPanelPagination
          page={page}
          pageSize={pageSize}
          total={total}
          totalPages={totalPages}
          rangeStart={rangeStart}
          rangeEnd={rangeEnd}
          pageSizeOptions={PAGE_SIZE_OPTIONS}
          loading={loading}
          itemNoun={{ singular: "session", plural: "sessions" }}
          onPrev={() => goToPage(page - 1)}
          onNext={() => goToPage(page + 1)}
          onPageSizeChange={changePageSize}
        />
      ) : null}

      {conversations.length > 0 ? (
        <ul className="space-y-2">
          {conversations.map((c) => (
            <li
              key={c.id}
              className="rounded-lg border border-border bg-[#0E1525] px-4 py-3 space-y-1"
            >
              <div className="flex items-start gap-3">
                <div className="flex-1 min-w-0 space-y-1">
                  {c.first_message ? (
                    <p className="text-sm text-white break-words">{c.first_message}</p>
                  ) : (
                    <p className="text-sm italic text-foreground/60">No messages yet</p>
                  )}
                  {/*
                    Fixed 3-column grid keeps agent / messages / started_at
                    vertically aligned across rows. Each cell truncates so a
                    long agent_id doesn't push the other columns sideways.
                    Memory panel uses the same template for consistency.
                  */}
                  <div className="grid grid-cols-[minmax(0,12rem)_minmax(0,8rem)_minmax(0,1fr)] gap-x-4 text-[11px] text-muted-foreground">
                    <span className="truncate" title={c.agent_id || undefined}>
                      {c.agent_id ? (
                        <>
                          agent: <code className="text-blue-300">{c.agent_id}</code>
                        </>
                      ) : (
                        <span className="text-muted-foreground/40">—</span>
                      )}
                    </span>
                    <span className="truncate">
                      messages:{" "}
                      <span className="tabular-nums text-foreground/85">
                        {c.messages.toLocaleString()}
                      </span>
                    </span>
                    <span className="truncate" title={c.started_at}>
                      started at:{" "}
                      <span className="tabular-nums text-foreground/85">
                        {formatDatapackCreatedAtUtc(c.started_at)}
                      </span>
                    </span>
                  </div>
                </div>
                {langfuseProjectId ? (
                  <button
                    type="button"
                    onClick={() => openConversationInObserve(c.id)}
                    className="shrink-0 inline-flex items-center gap-1 rounded-md border border-border bg-muted/10 px-2 py-1 text-[11px] font-medium text-foreground/80 hover:bg-muted/20 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-purple-500/40 transition-colors"
                    title="Open this conversation in the Observe (Langfuse) trace view"
                    aria-label={`Open conversation ${c.id} in Observe`}
                  >
                    <ExternalLink className="h-3 w-3" aria-hidden />
                    <span>Open in Observe</span>
                  </button>
                ) : null}
              </div>
            </li>
          ))}
        </ul>
      ) : null}

      {showInitialLoader ? (
        <div className="flex items-center justify-center gap-2 rounded-lg border border-dashed border-border bg-muted/5 px-4 py-10 text-xs text-muted-foreground">
          <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
          Loading conversations…
        </div>
      ) : null}
    </DatapackPanelShell>
  );
}
