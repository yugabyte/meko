// Header content for the WorkbenchLM page: title + a datapack picker
// across every pack the user can chat against (own, maintainer, or
// contributor — viewer is excluded because chat writes memories).
// Rendered inside `AppWorkspaceHeader`'s `leading` slot.

import { PlayCircle } from "lucide-react";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { cn } from "@/lib/utils";
import type { Datapack } from "@/lib/mekoApi";
import {
  formatDatapackShareGrantCamelCase,
  type DatapackShareGrant,
} from "@/lib/datapackShareGrants";

type Props = {
  /** Datapacks the signed-in user can use in the WorkbenchLM: own + shared
   *  with maintainer / contributor grant. Pre-filtered by the parent via
   *  `isDatapackGrantEligibleForWorkbenchLM`. */
  accessibleDatapacks: Datapack[];
  selectedDatapackName: string | null;
  onSelectDatapack: (name: string) => void;
  loading: boolean;
};

/** Small role chip shown to the right of each datapack name in the
 *  dropdown so the user can tell at a glance whether they're picking
 *  their own pack or one shared in. Colours match the canonical grant
 *  palette used elsewhere in the app. */
const GRANT_CHIP_CLASSES: Record<DatapackShareGrant, string> = {
  owner: "border-emerald-500/30 bg-emerald-500/10 text-emerald-300",
  maintainer: "border-purple-500/30 bg-purple-500/10 text-purple-300",
  contributor: "border-blue-500/30 bg-blue-500/10 text-blue-300",
  viewer: "border-border bg-muted/30 text-muted-foreground",
};

export function WorkbenchLMHeader({
  accessibleDatapacks,
  selectedDatapackName,
  onSelectDatapack,
  loading,
}: Props) {
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <div className="flex min-w-0 items-center gap-2.5">
        <PlayCircle className="h-4 w-4 shrink-0 text-purple-400" aria-hidden />
        <h1 className="text-lg font-semibold text-white">WorkbenchLM</h1>
        <Select
          value={selectedDatapackName ?? undefined}
          onValueChange={onSelectDatapack}
          disabled={loading || accessibleDatapacks.length === 0}
        >
          <SelectTrigger
            className="h-8 w-[260px] border-border/70 bg-[#1C2333] text-sm [&>span]:block [&>span]:min-w-0 [&>span]:flex-1 [&>span]:truncate [&>span]:text-left"
            aria-label="Select datapack"
            title={selectedDatapackName ?? undefined}
          >
            <SelectValue
              placeholder={
                loading
                  ? "Loading datapacks…"
                  : accessibleDatapacks.length === 0
                    ? "No datapacks available"
                    : "Select a datapack"
              }
            />
          </SelectTrigger>
          <SelectContent className="max-w-[min(28rem,calc(100vw-2rem))]">
            {accessibleDatapacks.map((dp) => (
              <SelectItem key={dp.datapack_id} value={dp.datapack_name}>
                <span className="inline-flex min-w-0 max-w-full items-center gap-2">
                  <span className="min-w-0 flex-1 truncate" title={dp.datapack_name}>
                    {dp.datapack_name}
                  </span>
                  {dp.grant ? (
                    <span
                      className={cn(
                        "shrink-0 rounded-full border px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wide",
                        GRANT_CHIP_CLASSES[dp.grant],
                      )}
                    >
                      {formatDatapackShareGrantCamelCase(dp.grant)}
                    </span>
                  ) : null}
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      </div>
      <p className="truncate text-xs text-muted-foreground" title={selectedDatapackName ?? undefined}>
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
