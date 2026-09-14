// Conversation lesson: mention a coding preference in a normal chat -- Meko
// extracts and remembers it automatically (postChat -> conversation_add_message
// -> server-side memory extraction, no direct memory_add call from the client).
// Wraps SeedContextChat, reframed with this lesson's own copy and examples.

import { useState } from "react";
import { MessageSquare } from "lucide-react";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { SeedContextChat } from "./SeedContextChat";
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
  const [sentOne, setSentOne] = useState(false);

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 5"
        title="Or just say it in passing"
        lede="Talk the way you'd talk to a teammate. Meko reads the conversation, decides what's worth keeping, and writes the memory itself."
        tags={["Conversation"]}
      />
      <SeedContextChat
        datapackId={session.datapackId}
        userId={session.userId}
        userToken={session.userToken}
        onTurnComplete={(steps) => {
          setSentOne(true);
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

      {sentOne && (
        // Extraction runs server-side after the turn, so the memory does not
        // appear the instant the reply does. Saying that is better than a
        // user watching an unchanged Memories count and concluding nothing
        // happened.
        <div className="flex items-start gap-3 rounded-lg border border-border bg-card p-4">
          <MessageSquare className="mt-0.5 h-4 w-4 shrink-0 text-context-conversation" aria-hidden />
          <div className="min-w-0">
            <p className="text-sm font-medium text-foreground">
              You didn't ask Meko to remember that
            </p>
            <p className="mt-0.5 text-xs text-muted-foreground">
              Meko reads the conversation and extracts what's worth keeping on its own. That runs after
              the reply, so a new memory appears shortly rather than instantly.
            </p>
          </div>
        </div>
      )}
    </div>
  );
}
