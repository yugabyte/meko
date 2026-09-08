// Conversation lesson: mention a coding preference in a normal chat -- Meko
// extracts and remembers it automatically (postChat -> conversation_add_message
// -> server-side memory extraction, no direct memory_add call from the client).
// Wraps SeedContextChat, reframed with this lesson's own copy and examples.

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { ContextSourceTracker } from "./ContextSourceTracker";
import { SeedContextChat } from "./SeedContextChat";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { XRayCall } from "../../lib/lessonTrace";

const EXAMPLE_PROMPTS = [
  "We always write a test before fixing a bug, not after.",
  "Watch out for SQL injection — never concatenate raw input into a query, always parameterize it.",
];

export function ConversationStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  return (
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto p-4">
      <ContextSourceTracker current="conversation" />
      <p className="text-sm text-muted-foreground">
        Mention a coding preference like you would to a teammate — Meko reads the
        conversation and remembers what matters, no extra step required.
      </p>
      <SeedContextChat
        datapackId={session.datapackId}
        userId={session.userId}
        userToken={session.userToken}
        onTurnComplete={(steps) => {
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
    </div>
  );
}
