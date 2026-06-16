// useMessageDecisionTrace — react-query wrapper around the single-message
// decision-trace endpoint (GET .../messages/:id/decision-trace).
//
// Why react-query here: a live WorkbenchLM session can rack up many
// assistant turns, and the trace for each is immutable once Langfuse has
// indexed it. Keying the query by (datapackId, conversationId, messageId)
// means:
//   - The footer-chip fetch (eager) and the DecisionTrace expand (lazy)
//     share ONE cache entry — expanding never re-hits the network.
//   - Re-opening a trace after scrolling away is a cache hit.

import { useQuery } from "@tanstack/react-query";
import {
  getDatapackMessageDecisionTrace,
  isMekoApiRequestError,
  type GetDatapackMessageDecisionTraceResponse,
} from "@/lib/api/mekoApi";

/** Stable prefix for the query key — exported so callers / tests can
 *  invalidate or assert against it without restating the string. */
export const MESSAGE_DECISION_TRACE_QUERY_KEY = "message-decision-trace";

type Params = {
  datapackName?: string | null;
  datapackId?: string | null;
  conversationId?: string | null;
  messageId?: string | null;
};

export function useMessageDecisionTrace(
  { datapackName, datapackId, conversationId, messageId }: Params,
  options?: { enabled?: boolean },
) {
  // Only fire when we have every id AND the caller opted in. Disabled
  // observers (e.g. user bubbles, or a collapsed trace) cost nothing.
  const enabled = (options?.enabled ?? true) && !!datapackName && !!conversationId && !!messageId;

  return useQuery<GetDatapackMessageDecisionTraceResponse>({
    queryKey: [
      MESSAGE_DECISION_TRACE_QUERY_KEY,
      datapackId ?? "",
      conversationId ?? "",
      messageId ?? "",
    ],
    // Own queryFn — the app-wide default queryFn does `fetch(key.join("/"))`,
    // which doesn't apply to our axios-based, datapack-scoped request.
    queryFn: () =>
      getDatapackMessageDecisionTrace(
        datapackName!,
        conversationId!,
        messageId!,
        datapackId ? { datapackId } : {},
      ),
    enabled,
    // staleTime means that the query will be refetched if it is older than 60 seconds on remount
    staleTime: 60_000,
    // The endpoint 404s until Langfuse finishes indexing the turn (a beat
    // after /chat returns). Treat ONLY 404 as "not ready yet" and retry a
    // few times so the eager footer-chip fetch self-heals; any other error
    // (403/500/network) fails immediately. ~8s window (4 × 2s).
    retry: (failureCount, error) =>
      isMekoApiRequestError(error) && error.httpStatus === 404 && failureCount < 4,
    retryDelay: 2000,
  });
}
