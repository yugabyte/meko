// Labs index: what Labs is, where it sits in the agentic development
// lifecycle, and the labs themselves — runnable ones first, then what's next.

import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";

import { AdlcStageStrip } from "../components/AdlcStageStrip";
import { LabCard } from "../components/LabCard";
import { UpcomingLabCard } from "../components/UpcomingLabCard";
import { UPCOMING_LABS } from "../data/adlcStages";
import { LAB_DEFINITIONS } from "../data/labDefinitions";

export default function LabsListPage() {
  return (
    <div className="flex h-full min-h-0 flex-col overflow-y-auto">
      <AppWorkspaceHeader
        leading={
          <p className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
            Agentic development lifecycle
          </p>
        }
      />

      <div className="flex flex-col gap-6 p-6">
        <div className="flex flex-col gap-3">
          <h1 className="text-2xl font-semibold">Labs</h1>
          {/* Wide enough for one line on a laptop, still capped so the
              sentence doesn't run the full width of an ultrawide display. */}
          <p className="max-w-7xl text-sm text-muted-foreground">
            Short, hands-on runs through a real ADLC stage. You build the context, an agent uses it, and
            you see exactly what it changed. Every lab ends with a path to your own repo.
          </p>
        </div>

        <AdlcStageStrip />

        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {LAB_DEFINITIONS.map((lab) => (
            <LabCard key={lab.id} lab={lab} />
          ))}
          {UPCOMING_LABS.map((lab) => (
            <UpcomingLabCard key={lab.title} lab={lab} />
          ))}
        </div>
      </div>
    </div>
  );
}
