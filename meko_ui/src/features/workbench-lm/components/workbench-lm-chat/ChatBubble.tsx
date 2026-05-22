// One bubble in the chat transcript — user (right, blue) or assistant
// (left, dark). Assistant bubbles render markdown and a meta row with
// Copy / latency / memories / llm / View trace.

import { Loader2 } from "lucide-react";
import { cn } from "@/lib/utils";
import { MarkdownMessage } from "@/components/markdown-message";
import { AgentReasoningBlock } from "./AgentReasoningBlock";
import { AssistantMetaRow } from "./AssistantMetaRow";
import type { Message } from "./types";

export function ChatBubble({
  message,
  onOpenTrace,
}: {
  message: Message;
  onOpenTrace?: () => void;
}) {
  const isUser = message.role === "user";
  return (
    <div className={cn("flex", isUser ? "justify-end" : "justify-start")}>
      <div
        className={cn(
          "max-w-[85%] rounded-2xl px-4 py-2.5 text-sm leading-relaxed",
          isUser
            ? "bg-blue-600/90 text-white"
            : "border border-border bg-card text-foreground",
        )}
      >
        {isUser ? (
          <div className="whitespace-pre-wrap break-words">{message.content}</div>
        ) : (
          <>
            {/*
              Agent reasoning sits ABOVE the answer per spec — gives the
              user a one-glance signal of what the agent grounded against
              before scrolling into the response. Self-hides when no
              memories / KB chunks / steps are present.
            */}
            <AgentReasoningBlock
              memoryHits={message.memoryHits ?? []}
              kbHits={message.kbHits ?? []}
              processingSteps={message.processingSteps ?? []}
            />
            <MarkdownMessage text={message.content} />
          </>
        )}
        {!isUser && (
          <AssistantMetaRow
            text={message.content}
            onOpenTrace={onOpenTrace}
            latencyMs={message.latencyMs}
            memoriesExtracted={message.memoriesExtracted}
            llmCalls={message.llmCalls}
          />
        )}
      </div>
    </div>
  );
}

/** Spinner placeholder shown while a turn is in flight. */
export function ThinkingBubble() {
  return (
    <div className="flex justify-start">
      <div className="flex max-w-[85%] items-center gap-2 rounded-2xl border border-border bg-card px-4 py-2.5 text-sm text-muted-foreground">
        <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
        Thinking…
      </div>
    </div>
  );
}
