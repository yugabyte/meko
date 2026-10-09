// Conversation lesson: mention a coding preference in a normal chat -- Meko
// extracts and remembers it automatically (postChat -> conversation_add_message
// -> server-side memory extraction, no direct memory_add call from the client).
// Wraps SeedContextChat, reframed with this lesson's own copy and examples.

import { useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { MessageSquare, Sparkles, Zap } from "lucide-react";

import { WarningBanner } from "@/components/ui/warning-banner";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { SeedContextChat } from "./SeedContextChat";
import {
  LAB_EXTRACTED_MEMORIES_QUERY_KEY,
  useExtractedMemories,
} from "../../hooks/useExtractedMemories";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { XRayCall } from "../../lib/lessonTrace";
import { BehindTheScenes } from "./BehindTheScenes";
import { LabStageHeader } from "./LabStageHeader";

const EXAMPLE_PROMPTS = [
  "We always write a test before fixing a bug, not after.",
  "Watch out for SQL injection. Never concatenate raw input into a query, always parameterize it.",
];

export function ConversationStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  const [turn, setTurn] = useState<{ conversationId: string; at: number } | null>(null);
  const queryClient = useQueryClient();

  const { memories: shown, waiting } = useExtractedMemories({
    datapackId: session.datapackId,
    conversationId: turn?.conversationId ?? null,
    startedAt: turn?.at ?? null,
  });

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 5"
        title="Add conversation from Slack or an agent harness"
        lede="The rules you never wrote down get said in passing. Drop one in the way it would arrive from Slack or a coding agent, and Meko reads it, decides what's worth keeping, and writes the memory itself."
        tags={["Conversation"]}
      />
      <SeedContextChat
        datapackId={session.datapackId}
        userId={session.userId}
        userToken={session.userToken}
        onTurnComplete={(steps, conversationId) => {
          setTurn({ conversationId, at: Date.now() });
          void queryClient.invalidateQueries({ queryKey: [LAB_EXTRACTED_MEMORIES_QUERY_KEY] });
          session.bumpRefreshTick();
          const calls: XRayCall[] = steps.map((step) => ({
            name: step.name,
            durationMs: step.duration_ms,
            status: step.status,
            detail: step.detail,
          }));
          session.recordLessonTrace({ lessonTitle: "Chat about a preference", calls });
        }}
        examplePrompts={EXAMPLE_PROMPTS}
        sampleOnly
      />

      {turn && (
        <BehindTheScenes
          concept="conversation"
          steps={[
            { icon: MessageSquare, label: "Kept verbatim", detail: "the thread itself, indexed" },
            { icon: Sparkles, label: "Read for facts", detail: "what looks durable, not chat" },
            { icon: Zap, label: "Memory written", detail: "you never asked it to" },
          ]}
        />
      )}

      {/* Extraction runs server-side after the reply, so there are a few
          seconds with nothing to show. Saying so beats a user watching an
          unchanged Memories count and concluding nothing happened. */}
      {turn && waiting && (
        <div className="flex items-start gap-3 rounded-lg border border-border bg-card p-4">
          <MessageSquare
            className="mt-0.5 h-4 w-4 shrink-0 text-context-conversation"
            aria-hidden
          />
          <div className="min-w-0">
            <p className="text-sm font-medium text-foreground">
              Meko is reading that for anything worth keeping
            </p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Extraction runs after the reply, so a new memory shows up shortly rather than
              instantly.
            </p>
          </div>
        </div>
      )}

      {/* Extraction can also produce nothing, or produce rows this listing
          does not tie back to the conversation. Saying so beats a step that
          shows only the chat after a turn. */}
      {turn && !waiting && shown.length === 0 && (
        <p className="text-sm text-muted-foreground">
          Nothing came back for that turn. Extraction keeps only what looks durable, so a passing
          remark often produces no memory at all.
        </p>
      )}

      {/* This conversation's memories where the listing says so, plus
          anything written since the turn -- see useExtractedMemories.
          Shown, not offered for a decision: keep/discard made this a triage
          screen, when the lesson is simply that Meko wrote one without being
          asked. Chapter 4 is where a memory gets a human decision. */}
      {shown.map((memory) => (
        <div
          key={memory.id}
          className="flex items-start gap-3 rounded-lg border border-context-memory/40 bg-context-memory/[0.06] p-4"
        >
          <Sparkles className="mt-0.5 h-4 w-4 shrink-0 text-context-memory" aria-hidden />
          <div className="min-w-0 flex-1">
            <p className="text-xs font-semibold uppercase tracking-wide text-context-memory">
              Memory extracted
            </p>
            <p className="mt-1 text-sm text-foreground">{memory.data}</p>
            <p className="mt-1 text-xs text-muted-foreground">
              from this conversation · you didn't ask
            </p>
          </div>
        </div>
      ))}

      {shown.length > 0 && (
        <WarningBanner tone="success">
          That's the whole point. The rule you'd have forgotten to write down is now context every
          agent can read, and the trace remembers which sentence produced it.
        </WarningBanner>
      )}
    </div>
  );
}
