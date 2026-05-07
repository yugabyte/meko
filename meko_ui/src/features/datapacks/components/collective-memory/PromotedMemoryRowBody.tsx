// Row body for a PromotedMemory: same text + agent line as MemoryRowBody, plus
// a small footer for the conversation that produced it and when it was promoted.

import type { PromotedMemory } from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";

type Props = {
  memory: PromotedMemory;
};

export function PromotedMemoryRowBody({ memory }: Props) {
  const data = memory.data ?? "";
  const agent = memory.agent_id ?? "";
  const conversationId = memory.conversation_id ?? "";
  const promotedAt = memory.promoted_to_kb_at ?? "";

  return (
    <div className="min-w-0 space-y-1">
      <p className="text-sm text-white break-words">
        {data || <span className="italic text-muted-foreground">(empty)</span>}
      </p>
      {(agent || conversationId || promotedAt) ? (
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-muted-foreground">
          {agent ? (
            <span>
              agent: <code className="text-blue-300">{agent}</code>
            </span>
          ) : null}
          {conversationId ? (
            <span title={conversationId}>
              conversation: <code className="text-slate-300">{conversationId}</code>
            </span>
          ) : null}
          {promotedAt ? (
            <span title={promotedAt}>
              promoted: <span className="text-foreground/80">{formatDatapackCreatedAtUtc(promotedAt)}</span>
            </span>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}
