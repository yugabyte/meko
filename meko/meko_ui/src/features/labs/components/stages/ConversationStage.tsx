// Conversation lesson: mention a coding preference in a normal chat -- Meko
// extracts and remembers it automatically (postChat -> conversation_add_message
// -> server-side memory extraction, no direct memory_add call from the client).
// Wraps SeedContextChat, reframed with this lesson's own copy and examples.

import { useCallback, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { MessageSquare } from "lucide-react";
import { deleteMemory, type Memory } from "@/lib/api/mekoApi";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { MemoryDecisionCard, type MemoryDecisionState } from "../demo-apps/MemoryDecisionCard";
import { SeedContextChat } from "./SeedContextChat";
import {
  LAB_EXTRACTED_MEMORIES_QUERY_KEY,
  useExtractedMemories,
} from "../../hooks/useExtractedMemories";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { XRayCall } from "../../lib/lessonTrace";
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
  // Per memory id, so deciding on one doesn't disable the others.
  const queryClient = useQueryClient();
  const [decisions, setDecisions] = useState<Record<string, MemoryDecisionState>>({});
  const [discarded, setDiscarded] = useState<Record<string, boolean>>({});
  const [kept, setKept] = useState<Record<string, boolean>>({});

  const { memories, waiting } = useExtractedMemories({
    datapackId: session.datapackId,
    conversationId: turn?.conversationId ?? null,
    startedAt: turn?.at ?? null,
  });

  // A discarded memory is gone from the listing, but its card has to stay on
  // screen to say so -- otherwise the tile vanishes on click with no word on
  // why. So the rendered set is the query's rows plus the ones this session
  // decided on, rather than a copy of the listing.
  const [decided, setDecided] = useState<Memory[]>([]);
  const byId = new Map<string, Memory>();
  for (const m of [...decided, ...memories]) byId.set(m.id, m);
  const shown = Array.from(byId.values());

  const discard = useCallback(
    async (memory: Memory) => {
      setDecisions((prev) => ({ ...prev, [memory.id]: "working" }));
      try {
        await deleteMemory(memory.id, session.datapackId as string);
        setDecided((prev) => (prev.some((m) => m.id === memory.id) ? prev : [...prev, memory]));
        setDiscarded((prev) => ({ ...prev, [memory.id]: true }));
        setDecisions((prev) => ({ ...prev, [memory.id]: "idle" }));
        // The pane beside this lesson counts memories, and the listing this
        // stage reads no longer holds the deleted row.
        session.bumpRefreshTick();
        void queryClient.invalidateQueries({ queryKey: [LAB_EXTRACTED_MEMORIES_QUERY_KEY] });
      } catch {
        // Best-effort: the lesson is about the decision existing, not about
        // recovering from a failed one. Returning to idle lets them retry.
        setDecisions((prev) => ({ ...prev, [memory.id]: "idle" }));
      }
    },
    [queryClient, session],
  );

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 5"
        title="Or just start a conversation"
        lede="Talk the way you'd talk to a teammate. Meko reads the conversation, decides what's worth keeping, and writes the memory itself."
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
      />

      {/* Extraction runs server-side after the reply, so there are a few
          seconds with nothing to show. Saying so beats a user watching an
          unchanged Memories count and concluding nothing happened. */}
      {turn && waiting && (
        <div className="flex items-start gap-3 rounded-lg border border-border bg-card p-4">
          <MessageSquare className="mt-0.5 h-4 w-4 shrink-0 text-context-conversation" aria-hidden />
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
          anything written since the turn -- see useExtractedMemories. */}
      {shown.map((memory) => (
        <MemoryDecisionCard
          key={memory.id}
          text={memory.data}
          badges={["memory extracted"]}
          caption="from this conversation · you didn't ask"
          primaryLabel="Keep it"
          primaryDoneLabel="Kept"
          secondaryLabel="Not useful"
          state={kept[memory.id] ? "done" : (decisions[memory.id] ?? "idle")}
          declined={discarded[memory.id] ?? false}
          outcome={
            discarded[memory.id] ? (
              <p className="rounded-md border border-border bg-card p-3 text-xs text-muted-foreground">
                Discarded. It's out of the datapack, so no agent will read it.
              </p>
            ) : (
              <p className="rounded-md border border-border bg-card p-3 text-xs text-muted-foreground">
                Kept. It's private to you until Chapter 4 promotes it.
              </p>
            )
          }
          // Local only. The obvious API for this, keep-as-memory, stamps
          // meko_promotion_user_action = 'keep_as_memory', which
          // PromoteMemoriesRepo.Promote then EXCLUDES -- so affirming a
          // memory here would make Chapter 4 unable to promote it, which is
          // the opposite of what this card says happens next.
          onPrimary={() => setKept((prev) => ({ ...prev, [memory.id]: true }))}
          onSecondary={() => void discard(memory)}
        />
      ))}
    </div>
  );
}
