// Per-turn metadata enrichment for the WorkbenchLM chat.
//
// The gateway's `POST /chat` returns a `message_id` but not the latency /
// memories_extracted / llm_calls breakdown. Those come from Langfuse via
// `GET /datapacks/:name/conversations/:id`. After each successful chat
// turn we fire this helper to fetch the thread, match the new message by
// id, and merge its meta into the assistant bubble.
//
// Best-effort by design: failures are logged and skipped. Langfuse
// ingestion can be a tick behind the gateway response, so we retry once
// after ~1.5s if the message isn't in the thread on the first call.

import { getDatapackConversation } from "@/lib/mekoApi";

type EnrichArgs = {
  datapackName: string;
  datapackId: string | null;
  conversationId: string;
  messageId: string;
  onMeta: (meta: {
    latencyMs?: number;
    memoriesExtracted?: number;
    llmCalls?: number;
  }) => void;
};

export async function enrichAssistantTurn(args: EnrichArgs): Promise<void> {
  // Strip dashes — `conversation_add_message` returns a hex UUID without
  // them, but the gateway might surface either form. Normalize both sides
  // before comparing.
  const normalize = (s: string) => s.replace(/-/g, "").toLowerCase();
  const targetId = normalize(args.messageId);

  const tryFetch = async (): Promise<boolean> => {
    try {
      const resp = await getDatapackConversation(
        args.datapackName,
        args.conversationId,
        // Pull a generous window so the new message lands in the page even
        // if a long history precedes it.
        { messagesLimit: 100, messagesOffset: 0 },
        args.datapackId ? { datapackId: args.datapackId } : {},
      );
      const found = resp.conversation.thread.find(
        (m) => normalize(m.message_id) === targetId,
      );
      if (!found) return false;
      args.onMeta({
        latencyMs: found.latency_ms,
        memoriesExtracted: found.memories_extracted,
        llmCalls: found.llm_calls,
      });
      return true;
    } catch (err) {
      // Log only — never user-visible. Thread fetch failing here doesn't
      // affect chat correctness.
      // eslint-disable-next-line no-console
      console.warn("Failed to fetch turn telemetry", err);
      return true; // don't retry on a hard error
    }
  };

  if (await tryFetch()) return;

  // Brief retry — Langfuse ingestion may be a tick behind the gateway's
  // response. One more shot at ~1.5s is usually enough.
  await new Promise((resolve) => setTimeout(resolve, 1500));
  await tryFetch();
}
