// Real numbers for the lab's dashboard stage, from the two sources that
// already exist: the datapack's conversation history and its token-savings
// summary. Nothing here is sample data.

import { useEffect, useMemo, useState } from "react";

import { listDatapackConversations, listMemories, type DatapackConversation } from "@/lib/api/mekoApi";
import { useDatapackTokenSavings } from "@/features/datapacks/hooks/useTokenSavings";

/** One page is plenty for a lab-scale datapack, and keeps this to a single
 *  request — the dashboard is a shape-of-your-activity view, not an audit. */
const CONVERSATION_PAGE_SIZE = 50;

export type ActivityPoint = { date: string; conversations: number };

export function useLabReviewDashboard(datapackId: string | null) {
  const [conversations, setConversations] = useState<DatapackConversation[]>([]);
  const [memoriesTotal, setMemoriesTotal] = useState(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const savings = useDatapackTokenSavings(datapackId);

  useEffect(() => {
    if (!datapackId) {
      setConversations([]);
      return;
    }
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    // Memories come from the memory list, NOT from summing each
    // conversation's `memories_added`: that field only counts rows
    // ATTRIBUTED to a conversation, so a fact saved straight through
    // /memory/add counts for nothing and the dashboard reported 0 while the
    // context pane beside it, reading this same endpoint, reported 7.
    Promise.all([
      listDatapackConversations(datapackId, { limit: CONVERSATION_PAGE_SIZE }, { signal: ac.signal }),
      listMemories({ datapack_id: datapackId, page: 1, page_size: 1 }, { signal: ac.signal }),
    ])
      .then(([page, memories]) => {
        if (ac.signal.aborted) return;
        setConversations(page.conversations);
        setMemoriesTotal(memories.total);
      })
      .catch((err: unknown) => {
        if (ac.signal.aborted) return;
        // A datapack with no Langfuse mapping yet answers 503 here. That's a
        // dashboard with no activity to show, not a broken page -- the
        // token-savings half still renders.
        setError(err instanceof Error ? err.message : "Couldn't load activity.");
      })
      .finally(() => {
        if (!ac.signal.aborted) setLoading(false);
      });
    return () => ac.abort();
  }, [datapackId]);

  const totals = useMemo(() => {
    return {
      conversations: conversations.length,
      memoriesAdded: memoriesTotal,
      llmCalls: conversations.reduce((sum, c) => sum + (c.llm_calls ?? 0), 0),
    };
  }, [conversations, memoriesTotal]);

  /** Conversations bucketed by calendar day, oldest first — the chart wants
   *  chronological order, the API returns newest first. */
  const activity = useMemo<ActivityPoint[]>(() => {
    const byDay = new Map<string, number>();
    for (const c of conversations) {
      const day = c.started_at.slice(0, 10);
      byDay.set(day, (byDay.get(day) ?? 0) + 1);
    }
    return Array.from(byDay.entries())
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([date, count]) => ({ date, conversations: count }));
  }, [conversations]);

  return {
    totals,
    activity,
    savings: savings.data ?? null,
    loading: loading || savings.isLoading,
    error,
  };
}
