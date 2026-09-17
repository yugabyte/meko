// The memories Meko extracted from one conversation, polled until they show
// up. Extraction runs server-side after the reply, so the lesson that taught
// Meko something has nothing to show for a few seconds and has to wait for it.

import { useQuery } from "@tanstack/react-query";

import { listMemories, type Memory } from "@/lib/api/mekoApi";

/** Fast enough that the lesson doesn't feel stalled, slow enough not to
 *  hammer the endpoint while extraction runs. */
const POLL_INTERVAL_MS = 3_000;

/** Give up after this long. Extraction usually lands inside 10s; past this
 *  it isn't coming, and polling forever would outlive the lesson. */
const POLL_TIMEOUT_MS = 45_000;

/** The created_at fallback compares a server timestamp against a browser
 *  clock, so it needs slack in the direction that keeps a real memory rather
 *  than one that hides it. */
const CLOCK_SKEW_MS = 30_000;

const PAGE_SIZE = 50;

/** Ids reach this hook from two systems that format them differently: the
 *  gateway returns a Langfuse trace id with the hyphens stripped, while the
 *  memories listing echoes whatever mem0 stored. Comparing them raw finds
 *  nothing even when they are the same id. */
function sameConversation(a: string | undefined, b: string | undefined): boolean {
  if (!a || !b) return false;
  return a.replace(/-/g, "").toLowerCase() === b.replace(/-/g, "").toLowerCase();
}

export const LAB_EXTRACTED_MEMORIES_QUERY_KEY = "lab-extracted-memories";

export type ExtractedMemories = {
  /** Memories this turn produced: rows the listing ties to `conversationId`,
   *  or failing that rows written since the turn began. In listing order,
   *  which for a paid tier is similarity-ranked rather than newest-first. */
  memories: Memory[];
  /** True while nothing has arrived yet and the deadline hasn't passed —
   *  what the caller shows a "reading that" state for. */
  waiting: boolean;
};

/**
 * Polls `GET /memories` for the rows extracted from `conversationId`.
 *
 * Scoped to one turn on purpose: the datapack's other memories are not this
 * lesson's doing, and showing them would credit the lesson with work the user
 * did earlier. The `created_at` fallback below is the exception, and it is
 * time-scoped rather than conversation-scoped -- a row it returns need not
 * belong to `conversationId`.
 *
 * Stops as soon as a row appears for THIS turn, or at POLL_TIMEOUT_MS, so a
 * datapack where extraction is off doesn't poll for the rest of the session.
 */
export function useExtractedMemories({
  datapackId,
  conversationId,
  startedAt,
}: {
  datapackId: string | null;
  conversationId: string | null;
  /** When the turn was sent, so the deadline is measured from the write
   *  rather than from whenever this component happened to mount. */
  startedAt: number | null;
}): ExtractedMemories {
  const enabled = Boolean(datapackId && conversationId);

  const { data } = useQuery({
    // startedAt is part of the key: SeedContextChat reuses one conversation
    // id for every turn, so without it the query stops after the first row
    // and a later turn's memory is never fetched.
    queryKey: [LAB_EXTRACTED_MEMORIES_QUERY_KEY, datapackId, conversationId, startedAt],
    enabled,
    // Always refetch: the whole point is watching for a row that did not
    // exist when the query first ran.
    staleTime: 0,
    queryFn: async ({ signal }) => {
      const res = await listMemories(
        { datapack_id: datapackId as string, page: 1, page_size: PAGE_SIZE },
        { signal },
      );
      const matched = res.memories.filter((m) =>
        sameConversation(m.conversation_id, conversationId ?? undefined),
      );
      if (matched.length > 0) return matched;

      // Fallback: a datapack whose rows carry no conversation_id at all (or
      // one this listing formats differently again) would otherwise show the
      // user nothing, in a lesson whose whole claim is that Meko wrote a
      // memory. Anything created since the turn was sent is that memory.
      if (startedAt === null) return [];
      return res.memories.filter((m) => {
        if (!m.created_at) return false;
        const at = Date.parse(m.created_at);
        return Number.isFinite(at) && at >= startedAt - CLOCK_SKEW_MS;
      });
    },
    refetchInterval: (query) => {
      const found = (query.state.data ?? []).length > 0;
      if (found) return false;
      const expired = startedAt !== null && Date.now() - startedAt > POLL_TIMEOUT_MS;
      return expired ? false : POLL_INTERVAL_MS;
    },
  });

  const memories = data ?? [];
  const expired = startedAt !== null && Date.now() - startedAt > POLL_TIMEOUT_MS;
  return { memories, waiting: enabled && memories.length === 0 && !expired };
}
