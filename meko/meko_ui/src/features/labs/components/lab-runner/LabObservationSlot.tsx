import { useIsMobile } from "@/hooks/use-mobile";
import { WorkbenchLMLiveObservationPanel } from "@/features/workbench-lm/components/WorkbenchLMLiveObservationPanel";
import type { DatapackShareGrant } from "@/lib/datapackShareGrants";

import type { LabDefinition } from "../../types";
import type { LessonTrace } from "../../lib/lessonTrace";
import { TokenSavingsPane } from "./TokenSavingsPane";
import { XRayPane } from "./XRayPane";

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
 * no-op there. TokenSavingsPane/XRayPane are collapsible sections stacked
 * below it, matching the reference screenshot's layout.
 */
export function LabObservationSlot({
  observation,
  datapackId,
  datapackName,
  datapackGrant,
  refreshTick,
  lastLessonTrace,
}: {
  observation: LabDefinition["observation"];
  datapackId: string | null;
  datapackName: string | null;
  datapackGrant: DatapackShareGrant | null;
  refreshTick: number;
  lastLessonTrace: LessonTrace | null;
}) {
  const isMobile = useIsMobile();
  if (observation.kind !== "workbench-live-observation") return null;
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden">
      <WorkbenchLMLiveObservationPanel
        datapackId={datapackId}
        datapackName={datapackName}
        datapackGrant={datapackGrant}
        paused={isMobile}
        refreshTick={refreshTick}
        layout="columns"
      />
      <TokenSavingsPane datapackId={datapackId} />
      <XRayPane trace={lastLessonTrace} />
    </div>
  );
}
