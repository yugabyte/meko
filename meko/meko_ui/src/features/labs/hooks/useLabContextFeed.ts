// Fetches what a lab's datapack currently holds — knowledge documents,
// private memories, promoted (collective) memories, and conversation count —
// for one datapack, refreshed after each lesson and on a slow timer.
//
// Two consumers: the runner's right pane displays it, and LabRunnerPage uses
// the counts to decide whether a lesson's gate is already satisfied. That
// second use is why this can't pause when the pane is hidden.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  getKnowledgeBaseDocuments,
  getKnowledgeBaseSummary,
  listDatapackConversations,
  listMemories,
  listPromotedMemories,
  type KnowledgeBaseDocument,
  type Memory,
  type PromotedMemory,
} from "@/lib/api/mekoApi";

/** Deliberately slower than WorkbenchLM's panel: a lab's pane is driven by
 *  `refreshTick` after each lesson, so the timer is only a safety net for
 *  writes that happen outside the lab (a teammate uploading a doc). */
const AUTO_REFRESH_INTERVAL_MS = 30_000;

/** The pane shows the most recent few of each source, not a full listing —
 *  it's an at-a-glance view of what the user just built, and the datapack
 *  detail page is where the complete lists live. */
const PAGE_SIZE = 6;

export type LabContextFeed = {
  knowledge: KnowledgeBaseDocument[];
  knowledgeTotal: number;
  memories: Memory[];
  memoriesTotal: number;
  collective: PromotedMemory[];
  collectiveTotal: number;
  /** Only a count — the pane doesn't list conversations, but a datapack that
   *  has any is evidence the conversation lesson's work exists. */
  conversationsTotal: number;
  /** Every chunk across every document in this datapack's knowledge base.
   *  The per-run context stats report only what a search RETRIEVED, so this
   *  is the one number that says how big the whole thing is. */
  chunksTotal: number | null;
  /** Characters per chunk, from the index's own config. Null when the summary
   *  is unavailable; callers fall back to file size. */
  chunkSize: number | null;
  loading: boolean;
};

const EMPTY: LabContextFeed = {
  knowledge: [],
  knowledgeTotal: 0,
  memories: [],
  memoriesTotal: 0,
  collective: [],
  collectiveTotal: 0,
  conversationsTotal: 0,
  chunksTotal: null,
  chunkSize: null,
  loading: false,
};

export function useLabContextFeed({
  datapackId,
  refreshTick,
  paused,
}: {
  datapackId: string | null;
  /** Bumped by the runner session after each lesson writes something. */
  refreshTick: number;
  /** Stops polling entirely. Only for callers that need neither the display
   *  nor the gate — the runner never pauses, because a hidden pane doesn't
   *  make a lesson's gate any less real. */
  paused: boolean;
}): LabContextFeed {
  const [feed, setFeed] = useState<LabContextFeed>(EMPTY);
  const [timerTick, setTimerTick] = useState(0);

  const load = useCallback(
    async (signal: AbortSignal) => {
      if (!datapackId) {
        setFeed(EMPTY);
        return;
      }
      setFeed((prev) => ({ ...prev, loading: true }));
      // Settled, not all: one source failing (a KB still provisioning, say)
      // shouldn't blank the other two.
      const [kb, mem, promoted, convos, summary] = await Promise.allSettled([
        getKnowledgeBaseDocuments(datapackId, { page: 1, page_size: PAGE_SIZE, signal }),
        listMemories({ datapack_id: datapackId, page: 1, page_size: PAGE_SIZE }, { signal }),
        listPromotedMemories({ datapack_id: datapackId, page: 1, page_size: PAGE_SIZE }, { signal }),
        listDatapackConversations(datapackId, { limit: 1 }, { signal }),
        getKnowledgeBaseSummary(datapackId, { signal }),
      ]);
      if (signal.aborted) return;
      setFeed({
        knowledge: kb.status === "fulfilled" ? kb.value.items : [],
        knowledgeTotal: kb.status === "fulfilled" ? kb.value.total : 0,
        memories: mem.status === "fulfilled" ? mem.value.memories : [],
        memoriesTotal: mem.status === "fulfilled" ? mem.value.total : 0,
        collective: promoted.status === "fulfilled" ? promoted.value.promoted_memories : [],
        collectiveTotal: promoted.status === "fulfilled" ? promoted.value.total : 0,
        conversationsTotal:
          convos.status === "fulfilled" ? convos.value.conversations.length : 0,
        chunksTotal: summary.status === "fulfilled" ? (summary.value.total_chunks ?? null) : null,
        chunkSize: summary.status === "fulfilled" ? (summary.value.chunk_size ?? null) : null,
        loading: false,
      });
    },
    [datapackId],
  );

  useEffect(() => {
    if (paused) return;
    const ac = new AbortController();
    void load(ac.signal);
    return () => ac.abort();
  }, [load, refreshTick, timerTick, paused]);

  useEffect(() => {
    if (paused || !datapackId) return;
    const id = window.setInterval(() => {
      // Page Visibility: a backgrounded tab shouldn't keep calling the API.
      if (document.visibilityState === "visible") setTimerTick((t) => t + 1);
    }, AUTO_REFRESH_INTERVAL_MS);
    return () => window.clearInterval(id);
  }, [paused, datapackId]);

  return useMemo(() => feed, [feed]);
}
