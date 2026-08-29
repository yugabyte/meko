import { useIsMobile } from "@/hooks/use-mobile";
import { DatapackTokenBurnTile } from "@/features/datapacks/components/efficiency/TokenBurnTile";
import { WorkbenchLMLiveObservationPanel } from "@/features/workbench-lm/components/WorkbenchLMLiveObservationPanel";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";

import type { LabDefinition } from "../../types";

/**
 * RIGHT pane of the lab runner. Reuses WorkbenchLMLiveObservationPanel via
 * props, not a fork — its polling is already datapack-agnostic, so this
 * needs nothing beyond passing through the lab's resolved datapack (plus
 * its grant, for the "Promote to Ko" gate) and pausing while this pane is
 * CSS-hidden on mobile (`hidden lg:flex` in LabRunnerPage), the same
 * problem WorkbenchLMPage solves with a separate MobileObservationSheet
 * instead. The one shared-component change this pane needed was adding
 * `flex-1` to that panel's root `<aside>` so it fills this stacked flex
 * column; harmless for its other two call sites (WorkbenchLMPage's
 * `md:contents` wrapper takes the aside out of the box model entirely;
 * MobileObservationSheet's sheet isn't a flex container), so `flex-1` is a
 * no-op there. DatapackTokenBurnTile is the same reuse: the existing
 * per-datapack Token Burn tile from the datapack detail page, unchanged,
 * just given this lab's datapack id.
 */
export function LabObservationSlot({
  observation,
  datapackId,
  datapackName,
  datapackGrant,
  refreshTick,
}: {
  observation: LabDefinition["observation"];
  datapackId: string | null;
  datapackName: string | null;
  datapackGrant: DatapackShareGrant | null;
  refreshTick: number;
}) {
  const isMobile = useIsMobile();
  if (observation.kind !== "workbench-live-observation") return null;
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      <DatapackTokenBurnTile datapackId={datapackId} />
      <WorkbenchLMLiveObservationPanel
        datapackId={datapackId}
        datapackName={datapackName}
        datapackGrant={datapackGrant}
        paused={isMobile}
        refreshTick={refreshTick}
      />
    </div>
  );
}
