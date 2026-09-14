// The knowledge chunks and memories a review actually retrieved, from the
// per-message decision trace api_server already serves.
//
// Separate from the review stream: the agent's `done` event reports how MANY
// chunks and memories it read, never which ones. The trace is where the
// content lives, keyed by the conversation and message the review produced.

import { useEffect, useState } from "react";

import {
  getApiErrorMessage,
  getDatapackConversation,
  getDatapackMessageDecisionTrace,
  type DatapackMessageDecisionTrace,
} from "@/lib/api/mekoApi";

/** Langfuse indexes the turn asynchronously, so the trace can 404 for a
 *  moment after a review finishes. Retry a few times before deciding it
 *  isn't coming rather than telling the user it failed. */
const RETRY_DELAYS_MS = [1_000, 3_000, 6_000];

export type ReviewRetrieval = {
  trace: DatapackMessageDecisionTrace | null;
  loading: boolean;
  /** Null while retries are still pending — an error here means it's settled. */
  error: string | null;
};

export function useReviewRetrieval({
  datapackId,
  conversationId,
}: {
  datapackId: string | null;
  /** The conversation the review wrote into, from the agent's `meta` event.
   *  Null until a review has completed. */
  conversationId: string | null;
}): ReviewRetrieval {
  const [state, setState] = useState<ReviewRetrieval>({ trace: null, loading: false, error: null });

  useEffect(() => {
    if (!datapackId || !conversationId) {
      setState({ trace: null, loading: false, error: null });
      return;
    }
    const ac = new AbortController();
    let attempt = 0;
    let timer: number | undefined;
    setState({ trace: null, loading: true, error: null });

    const run = async () => {
      try {
        // The agent reports a conversation, never a message. Meko's own
        // message ids live on the thread -- the ids PrReviewDemoApp holds are
        // React keys it minted locally, which no endpoint can resolve.
        const detail = await getDatapackConversation(
          datapackId,
          conversationId,
          { messagesLimit: 1 },
          { signal: ac.signal },
        );
        const reviewMessageId = detail.conversation.thread?.[0]?.message_id;
        if (!reviewMessageId) throw new Error("This review's message hasn't been indexed yet.");
        const res = await getDatapackMessageDecisionTrace(datapackId, conversationId, reviewMessageId, {
          signal: ac.signal,
        });
        if (ac.signal.aborted) return;
        setState({ trace: res.decision_trace, loading: false, error: null });
      } catch (err) {
        if (ac.signal.aborted) return;
        if (attempt < RETRY_DELAYS_MS.length) {
          timer = window.setTimeout(run, RETRY_DELAYS_MS[attempt]);
          attempt += 1;
          return;
        }
        setState({
          trace: null,
          loading: false,
          error: getApiErrorMessage(err, "Couldn't load what this review retrieved."),
        });
      }
    };
    void run();

    return () => {
      ac.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [datapackId, conversationId]);

  return state;
}
