// Forces an explicit datapack choice (no auto-select-first, unlike
// WorkbenchLMHeader's inline datapack Select) since a lab's demo app writes
// memories/KB content into whichever datapack is picked. Also frames the
// three context sources the next three lessons fill (absorbed from the old
// standalone ContextIntroStage).
//
// Creating is the primary path and selecting is the escape hatch: a lab run
// fills its datapack with sample standards and memories, which is not what
// someone wants landing in a datapack they already rely on.

import { useMemo, useState } from "react";
import { AlertCircle, ArrowRight, Loader2, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Datapack } from "@/lib/api/mekoApi";

import { suggestDatapackName } from "../../lib/datapackNameSuggestion";
import { DatapackConstructs } from "./DatapackConstructs";
import { LabNote } from "./LabNote";
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
  // `null` until the user picks a side, so the default below is re-derived on
  // every render. It has to be: `hasHitDatapackCount` comes from the
  // account-usage query, which reports false while it loads, so seeding state
  // from it once would open a capped user on a create form that can only
  // fail. The reuse case (a `heading` override) opens on the picker, where
  // choosing is the point.
  const [createChosen, setCreateChosen] = useState<boolean | null>(null);
  const showCreate = createChosen ?? (!heading && !hasHitDatapackCount);
  const setShowCreate = setCreateChosen;
  // Seeded, not placeholder text: a name the user can accept with one click
  // is the point, and they can still overwrite it.
  //
  // `null` means untouched, so the suggestion keeps re-deriving as the
  // datapack list loads. Seeding state once would take the suggestion before
  // the list arrived, offer a name that is already taken, and fail the create
  // -- the same stale-baseline shape useNewlyAdded had.
  const [typedName, setTypedName] = useState<string | null>(null);
  const suggestedName = useMemo(
    () => suggestDatapackName(datapacks.map((dp) => dp.datapack_name)),
    [datapacks],
  );
  const newName = typedName ?? suggestedName;
  const setNewName = setTypedName;
  const [createError, setCreateError] = useState<string | null>(null);

  const handleCreate = async () => {
    const trimmed = newName.trim();
    if (!trimmed) return;
    setCreateError(null);
    try {
      await onCreate(trimmed);
      setTypedName(null);
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
        lede={description ?? "Create a context for code review."}
      />

      {!heading && (
        <>
          <p className="text-sm text-muted-foreground">
            A datapack holds all the context for one project. Here are its constructs.
          </p>
          <DatapackConstructs />
        </>
      )}
      {showCreate ? (
        <div className="flex flex-col gap-3 rounded-lg border border-primary/40 bg-primary/[0.03] p-5">
          <div>
            <Label htmlFor="create-datapack-name" className="text-base">
              Create a datapack
            </Label>
            <p className="mt-1 text-xs text-muted-foreground">
              A fresh one keeps the sample standards and memories you are about to add out of
              anything you already rely on.
            </p>
          </div>
          <Input
            id="create-datapack-name"
            autoFocus
            placeholder="e.g. My team's datapack"
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void handleCreate();
            }}
          />
          {createError && (
            <p className="flex items-center gap-1.5 text-sm text-destructive">
              <AlertCircle className="h-4 w-4 shrink-0" /> {createError}
            </p>
          )}
          <Button
            size="lg"
            className="self-start"
            onClick={() => void handleCreate()}
            disabled={!newName.trim() || creating}
          >
            {creating ? <Loader2 className="h-4 w-4 animate-spin" /> : <Plus className="h-4 w-4" />}
            Create datapack
          </Button>
          <button
            type="button"
            className="self-start text-xs text-muted-foreground underline underline-offset-2 transition-colors hover:text-foreground"
            onClick={() => setShowCreate(false)}
          >
            Or use a datapack you already have
          </button>
        </div>
      ) : (
        <div className="flex flex-col gap-3 rounded-lg border border-border p-4">
          <Label htmlFor="create-datapack-select">Use an existing datapack</Label>
          <Select
            value={selectedId ?? undefined}
            onValueChange={(id) => {
              onSelect(id);
              onComplete();
            }}
            disabled={loading}
          >
            <SelectTrigger id="create-datapack-select" aria-label="Select datapack">
              <SelectValue
                placeholder={
                  loading
                    ? "Loading datapacks…"
                    : datapacks.length === 0
                      ? "No datapacks yet"
                      : "Select a datapack"
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
          {hasHitDatapackCount ? (
            <p className="text-sm text-muted-foreground">
              You've hit your datapack limit. Pick an existing one, or upgrade to create more.
            </p>
          ) : (
            <button
              type="button"
              className="self-start text-xs text-muted-foreground underline underline-offset-2 transition-colors hover:text-foreground"
              onClick={() => setShowCreate(true)}
            >
              Or create a new one
            </button>
          )}
        </div>
      )}
      {!heading && (
        <LabNote icon={ArrowRight}>
          Watch the right-hand pane as you go. That's the live datapack, not a mockup of one.
        </LabNote>
      )}
    </div>
  );
}
