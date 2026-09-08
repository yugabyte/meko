// Memory lesson: save a fact directly via POST /memory/add -- not routed
// through a conversation, unlike ConversationStage's chat-based extraction.

import { useState } from "react";
import { CheckCircle2, Loader2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Textarea } from "@/components/ui/textarea";
import { addMemory, getInferenceGatewayErrorMessage } from "@/lib/inferenceGateway";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { ContextSourceTracker } from "./ContextSourceTracker";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

const EXAMPLE_PREFERENCES = [
  "We squash-merge feature branches, never rebase-and-merge.",
  "Prefer composition over inheritance in new services.",
  "Always use parameterized queries — never build SQL by string concatenation.",
];

export function MemoryStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  const [text, setText] = useState("");
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }

  const canSave = text.trim().length > 0 && Boolean(session.userToken) && !saving;

  const handleSave = async () => {
    if (!session.userToken || !session.datapackId) return;
    setSaving(true);
    setError(null);
    const startedAt = Date.now();
    try {
      await addMemory({
        text: text.trim(),
        user_token: session.userToken,
        datapack_id: session.datapackId,
      });
      setSaved(true);
      setText("");
      session.bumpRefreshTick();
      session.recordLessonTrace({
        lessonTitle: "Add a memory",
        calls: [{ name: "memory_add", durationMs: Date.now() - startedAt, status: "ok" }],
      });
    } catch (err) {
      const message = getInferenceGatewayErrorMessage(err, "Couldn't save that memory.");
      setError(message);
      session.recordLessonTrace({
        lessonTitle: "Add a memory",
        calls: [{ name: "memory_add", durationMs: Date.now() - startedAt, status: "error", detail: message }],
      });
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-1 flex-col gap-4 overflow-y-auto p-4">
      <ContextSourceTracker current="memory" />
      <p className="text-sm text-muted-foreground">
        Tell Meko a coding preference directly — no chat needed, it's remembered
        immediately.
      </p>
      <div className="flex flex-wrap gap-2">
        {EXAMPLE_PREFERENCES.map((example) => (
          <button
            key={example}
            type="button"
            onClick={() => setText(example)}
            className="rounded-md border border-border bg-card px-2.5 py-1.5 text-left text-xs text-muted-foreground transition-colors hover:border-border/80 hover:text-foreground"
          >
            {example}
          </button>
        ))}
      </div>
      <Textarea
        value={text}
        onChange={(e) => setText(e.target.value)}
        placeholder="e.g. We always squash-merge feature branches."
        rows={4}
      />
      {error && <p className="text-sm text-destructive">{error}</p>}
      {saving ? (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <Loader2 className="h-3.5 w-3.5 animate-spin" aria-hidden />
          Meko is saving your memory…
        </p>
      ) : saved ? (
        <p className="flex items-center gap-1.5 text-xs text-emerald-700 dark:text-emerald-300">
          <CheckCircle2 className="h-3.5 w-3.5" aria-hidden />
          Done — check Private Memory on the right.
        </p>
      ) : null}
      <div>
        <Button size="sm" onClick={() => void handleSave()} disabled={!canSave}>
          Save memory
        </Button>
      </div>
    </div>
  );
}
