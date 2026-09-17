// Open-ended chat for building memory context — a trimmed copy of
// PrReviewDemoApp.tsx's send() loop: no fixed review prompt, no auto-fired
// first turn, no persona override (default WorkbenchLM playground-assistant
// framing is fine here). Every postChat turn already runs Meko's normal
// memory-extraction pipeline server-side (MCP conversation_add_message,
// infer=True) regardless of caller — existing infrastructure, not new.

import { useCallback, useState } from "react";
import { Loader2 } from "lucide-react";

import { MarkdownMessage } from "@/components/markdown-message";
import { getInferenceGatewayErrorMessage, postChat, type ProcessingStep } from "@/lib/inferenceGateway";
import { ChatInput } from "@/features/workbench-lm/components/workbench-lm-chat/ChatInput";
import type { Message } from "@/features/workbench-lm/components/workbench-lm-chat/types";

export function SeedContextChat({
  datapackId,
  userId,
  userToken,
  onTurnComplete,
  examplePrompts,
}: {
  datapackId: string;
  userId: string | null;
  userToken: string | null;
  /** Called after a successful turn with the MCP tool calls the gateway
   *  made (`res.processing_steps`) -- e.g. `memory_search`,
   *  `conversation_add_message` -- so the caller can surface them (X-Ray).
   *  The conversation id comes with them: extraction runs against that
   *  conversation, so it is how a caller finds what this turn taught Meko. */
  onTurnComplete: (steps: ProcessingStep[], conversationId: string) => void;
  /** Optional ready-made prompts rendered as clickable chips above the
   *  input — clicking one prefills the input rather than sending it, so
   *  the user can still edit before submitting. */
  examplePrompts?: string[];
}) {
  const [messages, setMessages] = useState<Message[]>([]);
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const [sessionId] = useState(() => crypto.randomUUID());
  const [conversationId, setConversationId] = useState<string | null>(null);
  const [sendError, setSendError] = useState<string | null>(null);

  const canChat = Boolean(userToken && userId);

  const send = useCallback(
    async (text: string) => {
      if (!userToken || !userId) return;
      setSending(true);
      setSendError(null);
      setMessages((prev) => [...prev, { id: crypto.randomUUID(), role: "user", content: text }]);
      try {
        const res = await postChat({
          message: text,
          user_token: userToken,
          user_id: userId,
          session_id: sessionId,
          datapack_id: datapackId,
          conversation_id: conversationId,
        });
        setConversationId(res.conversation_id);
        setMessages((prev) => [
          ...prev,
          { id: crypto.randomUUID(), role: "assistant", content: res.message, langfuseMessageId: res.message_id },
        ]);
        onTurnComplete(res.processing_steps ?? [], res.conversation_id);
      } catch (err) {
        setSendError(getInferenceGatewayErrorMessage(err));
      } finally {
        setSending(false);
      }
    },
    [userToken, userId, sessionId, conversationId, datapackId, onTurnComplete],
  );

  return (
    <div className="flex min-h-[16rem] flex-col rounded-md border border-border">
      <div className="flex-1 space-y-3 overflow-y-auto p-3">
        {messages.length === 0 && (
          <p className="text-sm text-muted-foreground">
            Tell Meko about your team's conventions, past incidents, anything worth remembering. Every
            message here runs through the same memory pipeline the review chat uses.
          </p>
        )}
        {messages.map((m) => (
          <div
            key={m.id}
            className={
              m.role === "user"
                ? "ml-auto max-w-[85%] rounded-md bg-primary/10 p-3 text-sm"
                : "rounded-md border border-border bg-card p-3 text-sm"
            }
          >
            {m.role === "assistant" ? <MarkdownMessage text={m.content} /> : m.content}
          </div>
        ))}
        {sending && <Loader2 className="h-4 w-4 animate-spin text-muted-foreground" />}
        {sendError && <p className="text-sm text-destructive">{sendError}</p>}
      </div>
      {examplePrompts && examplePrompts.length > 0 && (
        <div className="flex flex-wrap gap-2 border-t border-border p-2">
          {examplePrompts.map((prompt) => (
            <button
              key={prompt}
              type="button"
              onClick={() => setInput(prompt)}
              className="rounded-md border border-border bg-card px-2.5 py-1.5 text-left text-xs text-muted-foreground transition-colors hover:border-border/80 hover:text-foreground"
            >
              {prompt}
            </button>
          ))}
        </div>
      )}
      <ChatInput
        compact
        value={input}
        onChange={setInput}
        onSubmit={() => {
          const text = input.trim();
          if (!text) return;
          setInput("");
          void send(text);
        }}
        disabled={!canChat}
        sending={sending}
        blockedReason={canChat ? null : "Setting up this lab's datapack..."}
      />
    </div>
  );
}
