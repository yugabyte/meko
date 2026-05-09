// Header content for the Playground page: title, owner-datapack picker, and selected datapack label.
// Rendered inside `AppWorkspaceHeader`'s `leading` slot.

import { PlayCircle } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import type { Datapack } from "@/lib/mekoApi";

type Props = {
  ownedDatapacks: Datapack[];
  selectedDatapackName: string | null;
  onSelectDatapack: (name: string) => void;
  loading: boolean;
};

export function PlaygroundHeader({
  ownedDatapacks,
  selectedDatapackName,
  onSelectDatapack,
  loading,
}: Props) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="flex min-w-0 items-center gap-2.5">
        <PlayCircle className="h-4 w-4 shrink-0 text-purple-400" aria-hidden />
        <h1 className="text-lg font-semibold text-white">Playground</h1>
        <Select
          value={selectedDatapackName ?? undefined}
          onValueChange={onSelectDatapack}
          disabled={loading || ownedDatapacks.length === 0}
        >
          <SelectTrigger
            className="h-8 w-[220px] border-border/70 bg-[#1C2333] text-sm"
            aria-label="Select datapack"
          >
            <SelectValue
              placeholder={
                loading
                  ? "Loading datapacks…"
                  : ownedDatapacks.length === 0
                    ? "No owned datapacks"
                    : "Select a datapack"
              }
            />
          </SelectTrigger>
          <SelectContent>
            {ownedDatapacks.map((dp) => (
              <SelectItem key={dp.datapack_id} value={dp.datapack_name}>
                {dp.datapack_name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <p className="text-xs text-muted-foreground">
        {selectedDatapackName ? (
          <>
            Datapack:{" "}
            <span className="font-medium text-white">{selectedDatapackName}</span>
          </>
        ) : (
          "Pick a datapack to start exploring."
        )}
      </p>
    </div>
  );
}
