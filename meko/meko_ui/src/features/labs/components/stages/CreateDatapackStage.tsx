// Stage 1: forces an explicit datapack choice (no auto-select-first, unlike
// WorkbenchLMHeader's inline datapack Select) since a lab's demo app writes
// memories/KB content into whichever datapack is picked.

import { useState } from "react";
import { AlertCircle, Loader2, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import type { Datapack } from "@/lib/api/mekoApi";

export function CreateDatapackStage({
  datapacks,
  loading,
  selectedId,
  onSelect,
  onCreate,
  creating,
  hasHitDatapackCount,
  onContinue,
}: {
  datapacks: Datapack[];
  loading: boolean;
  selectedId: string | null;
  onSelect: (id: string) => void;
  onCreate: (name: string) => Promise<void>;
  creating: boolean;
  hasHitDatapackCount: boolean;
  onContinue: () => void;
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
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : "Failed to create the datapack.");
    }
  };

  return (
    <div className="mx-auto flex w-full max-w-md flex-1 flex-col justify-center gap-4 overflow-y-auto p-6">
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
            <Select value={selectedId ?? undefined} onValueChange={onSelect} disabled={loading}>
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
              <SelectContent>
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
              You've hit your datapack limit — pick an existing one, or upgrade to create more.
            </p>
          )}
        </div>
      )}
      <Button onClick={onContinue} disabled={!selectedId}>
        Continue to Seed Context
      </Button>
    </div>
  );
}
