// Chapter 4's middle step: take one private memory from Chapter 1 and make it
// the whole datapack's. Promotion is the only thing in the lab that changes
// what OTHER people's agents read, which is why it gets its own step and its
// own deliberate button.

import { useCallback, useState } from "react";
import { Users } from "lucide-react";

import { promoteMemories } from "@/lib/api/mekoApi";

import type { LabContextFeed } from "../../hooks/useLabContextFeed";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { MemoryDecisionCard, type MemoryDecisionState } from "../demo-apps/MemoryDecisionCard";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageHeader } from "./LabStageHeader";

/** Enough to choose from without turning the step into a memory manager. */
const MAX_CANDIDATES = 3;

export function PromoteLearningStage({
  session,
  feed,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  /** Already loaded for the pane beside this stage, so the candidates cost
   *  no extra request. */
  feed: LabContextFeed;
  onGoToCreateDatapack: () => void;
}) {
  const [decisions, setDecisions] = useState<Record<string, MemoryDecisionState>>({});
  const [kept, setKept] = useState<Record<string, boolean>>({});
  const [failed, setFailed] = useState<Record<string, boolean>>({});

  const promote = useCallback(
    async (memoryId: string | undefined, key: string) => {
      if (!session.datapackId || !session.userId || !memoryId) return;
      setDecisions((prev) => ({ ...prev, [key]: "working" }));
      try {
        const result = await promoteMemories({
          user_id: session.userId,
          datapack_id: session.datapackId,
          memory_ids: [memoryId],
        });
        // A 200 is not a promotion. PromoteMemoriesRepo skips rows that are
        // another member's, stamped keep_as_memory, or missing a
        // conversation id, and reports them in not_found_ids with nothing
        // inserted -- the same check the Learnings UI makes.
        const promoted = result.inserted_ids.length + result.updated_ids.length > 0;
        if (!promoted) {
          setFailed((prev) => ({ ...prev, [key]: true }));
          setDecisions((prev) => ({ ...prev, [key]: "idle" }));
          return;
        }
        setDecisions((prev) => ({ ...prev, [key]: "done" }));
        // The pane's Collective count is the visible half of this.
        session.bumpRefreshTick();
      } catch {
        setFailed((prev) => ({ ...prev, [key]: true }));
        setDecisions((prev) => ({ ...prev, [key]: "idle" }));
      }
    },
    [session],
  );

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  // Only this user's own memories. The feed lists the whole datapack, and
  // after the collaborate step that includes other members' rows --
  // promoteMemories matches payload->>'user_id', so promoting one of those
  // would report success having inserted nothing.
  const candidates = feed.memories
    .filter((m) => !m.user_id || m.user_id === session.userId)
    .slice(0, MAX_CANDIDATES);

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 2"
        title="Let the whole team learn it once"
        lede="The memories you added in Chapter 1 are private to you. Promote one and every agent with access to this datapack reads it, including the one about to write the fix."
        tags={["Collective memory"]}
      />

      {candidates.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No private memories on this datapack yet. Chapter 1 adds one, either directly or by
          extracting it from a conversation.
        </p>
      ) : (
        candidates.map((memory, i) => {
          // A row with no id shares the record key of every other such row,
          // so one decision would settle all of them -- and it cannot be
          // promoted anyway, since the API takes memory_ids.
          const key = memory.id || `row-${i}`;
          const promotable = Boolean(memory.id);
          return (
          <MemoryDecisionCard
            key={key}
            text={memory.data}
            badges={["private memory", "needs a human"]}
            caption="yours until you promote it"
            primaryLabel="Promote to collective memory"
            primaryDoneLabel="Promoted"
            secondaryLabel="Keep private"
            state={promotable ? (decisions[key] ?? "idle") : "unavailable"}
            // Declining is its own answer: it locks the card without ever
            // claiming the memory was promoted.
            declined={(kept[key] ?? false) || (failed[key] ?? false)}
            outcome={
              failed[key] ? (
                <p className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-xs text-destructive">
                  That memory could not be promoted. It may belong to another member of this
                  datapack, or predate the conversation record promotion needs.
                </p>
              ) : kept[key] ? (
                <p className="rounded-md border border-border bg-card p-3 text-xs text-muted-foreground">
                  Left private. It still reaches your own agents on this datapack, just not anyone
                  else's.
                </p>
              ) : (
                <p className="flex items-start gap-2 rounded-md border border-context-knowledge/40 bg-context-knowledge-soft p-3 text-xs text-context-knowledge">
                  <Users className="mt-0.5 h-3.5 w-3.5 shrink-0" aria-hidden />
                  <span>
                    Every agent on this datapack reads it from its next query. The reviewer learned
                    it, you approved it, and the agent writing the fix starts with it.
                  </span>
                </p>
              )
            }
            onPrimary={() => void promote(memory.id, key)}
            onSecondary={() => setKept((prev) => ({ ...prev, [key]: true }))}
          />
          );
        })
      )}
    </div>
  );
}
