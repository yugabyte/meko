// Memory lesson: save a fact directly via POST /memory/add -- not routed
// through a conversation, unlike ConversationStage's chat-based extraction.

import { useState } from "react";
import { Loader2, Search, Sparkles, Zap } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { WarningBanner } from "@/components/ui/warning-banner";
import { addMemory, getInferenceGatewayErrorMessage } from "@/lib/inferenceGateway";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { BehindTheScenes } from "./BehindTheScenes";
import { LabStageHeader } from "./LabStageHeader";

/** Deliberately none of them about SQL: the knowledge base already covers
 *  that, and the point of this lesson is the rule that never made it into a
 *  document. */
const EXAMPLE_PREFERENCES = [
  "We squash-merge feature branches, never rebase-and-merge.",
  "Date-range params are capped at 90 days on billing endpoints.",
  "Prefer composition over inheritance in new services.",
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
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <LabStageHeader
        eyebrow="Chapter 1 · Step 4"
        title="Add another rule as memory"
        lede="Any extra rules can be added as memory: the ones that never made it into a document."
        tags={["Memory"]}
      />

      {/* Stacked, each hugging its own text: three rules read as three rules
          this way. Wrapped into a row they read as one run-on sentence, and
          the shorter two end up sharing a line. */}
      <div className="flex flex-col items-start gap-2">
        {EXAMPLE_PREFERENCES.map((example) => (
          <button
            key={example}
            type="button"
            onClick={() => setText(example)}
            className="rounded-full border border-border bg-card px-3.5 py-1.5 text-left text-xs text-muted-foreground transition-colors hover:border-context-memory/60 hover:text-foreground"
          >
            {example}
          </button>
        ))}
      </div>

      {/* A memory is one sentence, so a single-line field says so. A textarea
          invites a paragraph, which extraction then has to guess its way
          through. */}
      <div className="flex items-center gap-2">
        <Input
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter" && canSave) void handleSave();
          }}
          placeholder="e.g. We cap billing date ranges at 90 days."
          className="h-10 flex-1"
        />
        <Button onClick={() => void handleSave()} disabled={!canSave} className="h-10 shrink-0">
          {saving ? <Loader2 className="h-4 w-4 animate-spin" aria-hidden /> : null}
          Save memory
        </Button>
      </div>
      {error && <p className="text-sm text-destructive">{error}</p>}

      {/* Part of the action, not the aftermath: who can read a memory is
          something to know before saving one, so it sits with the field
          rather than after the result. */}
      <div className="flex items-start gap-3 rounded-lg border border-border bg-card p-4">
        <Sparkles className="mt-0.5 h-4 w-4 shrink-0 text-context-memory" aria-hidden />
        <div className="min-w-0 flex-1">
          <p className="text-sm font-medium text-foreground">Scope</p>
          <p className="mt-0.5 text-xs text-muted-foreground">
            Private to you until you promote it. Promote one and every agent on this datapack reads it.
          </p>
        </div>
        {/* Same memory blue the datapack detail page tags a private memory
            with, so "private" means one colour wherever it appears. */}
        <Badge
          variant="outline"
          className="shrink-0 border-context-memory/40 bg-context-memory/10 text-[10px] text-context-memory"
        >
          private
        </Badge>
      </div>

      {(saving || saved) && (
        <BehindTheScenes
          concept="memory"
          steps={[
            { icon: Sparkles, label: "Saved", detail: "one fact, written as-is" },
            { icon: Zap, label: "Indexed inline", detail: "no re-embed of the file" },
            { icon: Search, label: "Retrievable", detail: "on the very next query" },
          ]}
        />
      )}

      {/* Last: the band that says the lesson worked closes it. */}
      {saved && (
        <WarningBanner tone="success">
          Live immediately. No re-index, no rebuild, and the next agent query already sees it.
        </WarningBanner>
      )}
    </div>
  );
}
