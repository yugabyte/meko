// Conversations tab content: top-K preview of Langfuse sessions for the
// selected datapack, mirroring the MemoryReadOnlySummaryPanel layout.
//
// Each row is clickable — clicking deep-links into the full conversation
// viewer (`/datapacks/:name/conversations/:conversationId`) with that
// session pre-selected. A "View all conversations" button at the bottom
// opens the same viewer with no selection (lands on the newest session).
//
// Pagination intentionally lives only in the full viewer; this panel
// always shows the newest CONVERSATIONS_PREVIEW_SIZE rows.

import { useCallback, useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { ExternalLink, Loader2, MessageSquare } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  getApiErrorMessage,
  listDatapackConversations,
  type DatapackConversation,
} from "@/lib/mekoApi";
import {
  ROUTES,
  datapackConversationsPath,
} from "@/app/routes/paths";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import DatapackPanelShell from "@/features/datapacks/components/datapack-detail/DatapackPanelShell";

/** How many of the newest conversations to show in the in-tab preview.
 *  Matches the "top 3-4" call-out from product; we use 4 so the panel
 *  visually mirrors the memories preview. */
export const CONVERSATIONS_PREVIEW_SIZE = 7;

type Props = {
  datapackName: string;
  datapackId?: string;
  /** Authoritative session count for this datapack, sourced from
   *  `dp.conversations_count` on the parent. Used purely for the
   *  "· N sessions" header chip and to decide whether to render the
   *  "View all" button. */
  total: number;
  className?: string;
};

export default function ConversationsPanel({ datapackName, datapackId, total, className }: Props) {
  const navigate = useNavigate();
  const [conversations, setConversations] = useState<DatapackConversation[]>([]);
  /** Langfuse project_id for this datapack. Empty until first fetch
   *  resolves with a mapping. Drives the per-row "Open in Observe"
   *  shortcut (separate from the row-click → native viewer flow). */
  const [langfuseProjectId, setLangfuseProjectId] = useState<string>("");
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [hasFetched, setHasFetched] = useState(false);

  const load = useCallback(
    async (signal?: AbortSignal) => {
      setLoading(true);
      setError(null);
      try {
        const resp = await listDatapackConversations(
          datapackName,
          { limit: CONVERSATIONS_PREVIEW_SIZE, offset: 0 },
          { signal, datapackId },
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
    [datapackName, datapackId],
  );

  useEffect(() => {
    const ac = new AbortController();
    setConversations([]);
    setLangfuseProjectId("");
    setHasFetched(false);
    void load(ac.signal);
    return () => ac.abort();
  }, [datapackName, datapackId, load]);

  const openConversation = useCallback(
    (conversationId: string) => {
      navigate(datapackConversationsPath(datapackName, conversationId));
    },
    [datapackName, navigate],
  );

  /** Secondary "open in raw Langfuse iframe" shortcut. Stops propagation
   *  so it doesn't also fire the row-click navigation to the native
   *  viewer. */
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

  const showEmpty = !error && hasFetched && total === 0;
  const showInitialLoader = loading && conversations.length === 0 && !error;
  const showViewAll = total > CONVERSATIONS_PREVIEW_SIZE;

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
            <span className="font-normal text-muted-foreground"> · …</span>
          )}
        </>
      }
      description={
        <>
          Recent conversations in{" "}
          <span className="font-medium text-white">{datapackName}</span>
        </>
      }
      onRefresh={() => void load()}
      refreshLoading={loading}
      errors={[error]}
    >
      {showEmpty ? (
        <p className="text-xs text-muted-foreground">
          No conversations recorded for this datapack yet.
        </p>
      ) : null}

      {conversations.length > 0 ? (
        <>
          <ul className="space-y-2">
            {conversations.map((c) => {
              const headline = c.title || c.first_message;
              return (
                <li key={c.id}>
                  <button
                    type="button"
                    onClick={() => openConversation(c.id)}
                    className="block w-full rounded-lg border border-border bg-[#0E1525] px-4 py-3 text-left transition-colors hover:border-purple-500/40 hover:bg-[#141C2E] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-purple-500/40"
                    aria-label={`Open conversation ${c.id}`}
                  >
                    <div className="flex items-start gap-3">
                      <div className="flex-1 min-w-0 space-y-1">
                        {headline ? (
                          <p className="text-sm text-white break-words line-clamp-2">{headline}</p>
                        ) : (
                          <p className="text-sm italic text-foreground/60">No messages yet</p>
                        )}
                        {/*
                          Fixed 4-column grid keeps agent / messages /
                          memories / started_at vertically aligned across
                          rows. Each cell truncates so a long agent_id
                          doesn't push the other columns sideways.
                        */}
                        <div className="grid grid-cols-[minmax(0,12rem)_minmax(0,7rem)_minmax(0,7rem)_minmax(0,1fr)] gap-x-4 text-[11px] text-muted-foreground">
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
                              {(c.messages ?? 0).toLocaleString()}
                            </span>
                          </span>
                          <span className="truncate">
                            memories:{" "}
                            <span className="tabular-nums text-blue-300">
                              {(c.memories_added ?? 0).toLocaleString()}
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
                          onClick={(e) => {
                            e.stopPropagation();
                            openConversationInObserve(c.id);
                          }}
                          className="shrink-0 inline-flex items-center gap-1 rounded-md border border-border bg-muted/10 px-2 py-1 text-[11px] font-medium text-foreground/80 hover:bg-muted/20 hover:text-white focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-purple-500/40 transition-colors"
                          title="Open this conversation in the raw Langfuse trace view"
                          aria-label={`Open conversation ${c.id} in Observe`}
                        >
                          <ExternalLink className="h-3 w-3" aria-hidden />
                          <span>Observe</span>
                        </button>
                      ) : null}
                    </div>
                  </button>
                </li>
              );
            })}
          </ul>
          {showViewAll ? (
            <div className="flex justify-center pt-1">
              <Button
                variant="outline"
                className="min-w-[18rem] max-w-full border-border bg-background/80 px-6 py-2.5 text-sm sm:min-w-[22rem]"
                asChild
              >
                <Link to={datapackConversationsPath(datapackName)}>
                  View all conversations
                </Link>
              </Button>
            </div>
          ) : null}
        </>
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
