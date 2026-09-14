// Forces an explicit datapack choice (no auto-select-first, unlike
// WorkbenchLMHeader's inline datapack Select) since a lab's demo app writes
// memories/KB content into whichever datapack is picked. Also frames the
// three context sources the next three lessons fill (absorbed from the old
// standalone ContextIntroStage).

import { useState } from "react";
import { AlertCircle, Loader2, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { Datapack } from "@/lib/api/mekoApi";

import { ContextSourceCards } from "./ContextSourceCards";
import { LabStageHeader } from "./LabStageHeader";

export function CreateDatapackStage({
  datapacks,
  loading,
  selectedId,
  onSelect,
  onCreate,
  creating,
  hasHitDatapackCount,
  onComplete,
  heading,
  description,
}: {
  datapacks: Datapack[];
  loading: boolean;
  selectedId: string | null;
  onSelect: (id: string) => void;
  onCreate: (name: string) => Promise<void>;
  creating: boolean;
  hasHitDatapackCount: boolean;
  /** Satisfies this stage's gate once a datapack is actually chosen. */
  onComplete: () => void;
  /** Override the default heading/lede. Set by the later reuses of this
   *  component (the "choose-datapack-again" case in LabStageSlot.tsx --
   *  there is no separate ChooseDatapackAgainStage) where the user already
   *  has a datapack and needs to know why they're being asked again. Those
   *  reuses also skip the context-source framing below, which only makes
   *  sense the first time. */
  heading?: string;
  description?: string;
}) {
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");
  const [createError, setCreateError] = useState<string | null>(null);

  const handleCreate = async () => {
    const trimmed = newName.trim();
    if (!trimmed) return;
    setCreateError(null);
    try {
      await onCreate(trimmed);
      setNewName("");
      setShowCreate(false);
      onComplete();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : "Failed to create the datapack.");
    }
  };

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow={heading ? "Pick a datapack" : "Chapter 1 · Step 2"}
        title={heading ?? "One datapack holds the context"}
        lede={
          description ??
          "A datapack is the unit agents read from and write to: a knowledge base, memories, and the conversations that produced them. You'll fill this one over the next three steps."
        }
      />
      {showCreate ? (
        <div className="flex flex-col gap-3 rounded-md border border-border p-4">
          <Label htmlFor="create-datapack-name">New datapack name</Label>
          <Input
            id="create-datapack-name"
            autoFocus
            placeholder="e.g. My team's datapack"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void handleCreate();
              if (e.key === "Escape") setShowCreate(false);
            }}
          />
          {createError && (
            <p className="flex items-center gap-1.5 text-sm text-destructive">
              <AlertCircle className="h-4 w-4 shrink-0" /> {createError}
            </p>
          )}
          <div className="flex gap-2">
            <Button onClick={() => void handleCreate()} disabled={!newName.trim() || creating}>
              {creating && <Loader2 className="h-4 w-4 animate-spin" />}
              Create
            </Button>
            <Button variant="ghost" onClick={() => setShowCreate(false)}>
              Cancel
            </Button>
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-3 rounded-md border border-border p-4">
          <Label htmlFor="create-datapack-select">Select a datapack</Label>
          <div className="flex items-center gap-2">
            <Select
              value={selectedId ?? undefined}
              onValueChange={(id) => {
                onSelect(id);
                onComplete();
              }}
              disabled={loading}
            >
              <SelectTrigger
                id="create-datapack-select"
                className="flex-1"
                aria-label="Select datapack"
              >
                <SelectValue
                  placeholder={
                    loading ? "Loading datapacks…" : datapacks.length === 0 ? "No datapacks yet" : "Select a datapack"
                  }
                />
              </SelectTrigger>
              <SelectContent className="max-h-[300px]">
                {datapacks.map((dp) => (
                  <SelectItem key={dp.datapack_id} value={dp.datapack_id}>
                    {dp.datapack_name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button
              variant="outline"
              className="gap-1"
              onClick={() => setShowCreate(true)}
              disabled={hasHitDatapackCount}
              title={hasHitDatapackCount ? "You've hit your datapack limit -- upgrade to create more." : undefined}
            >
              <Plus className="h-4 w-4" />
              New
            </Button>
          </div>
          {hasHitDatapackCount && (
            <p className="text-sm text-muted-foreground">
              You've hit your datapack limit. Pick an existing one, or upgrade to create more.
            </p>
          )}
        </div>
      )}
      {!heading && (
        <>
          <ContextSourceCards />
          <p className="text-xs text-muted-foreground">
            Watch the right-hand pane as you go. That's the live datapack, not a mockup of one.
          </p>
        </>
      )}
    </div>
  );
}
