import { WorkbenchLMLiveObservationPanel } from "@/features/workbench-lm/components/WorkbenchLMLiveObservationPanel";

import type { LabDefinition } from "../../types";

/**
 * RIGHT pane of the lab runner. Reuses WorkbenchLMLiveObservationPanel
 * as-is via props — no fork. Its polling is already datapack-agnostic, so
 * this needs nothing beyond passing through the lab's resolved datapack.
 */
export function LabObservationSlot({
  observation,
  datapackId,
  datapackName,
  refreshTick,
}: {
  observation: LabDefinition["observation"];
  datapackId: string | null;
  datapackName: string | null;
  refreshTick: number;
}) {
  if (observation.kind !== "workbench-live-observation") return null;
  return (
    <WorkbenchLMLiveObservationPanel
      datapackId={datapackId}
      datapackName={datapackName}
      refreshTick={refreshTick}
    />
  );
}
