// Stage 3: standalone now — no longer gated behind a completed review.
// DeployAgentPanel itself is unchanged; only its reachability changed.
// Leads with ReviewContextComparison so deploying isn't a bare picker — it
// sets the premise (what the context you just built actually changes about
// a review) before asking you to choose an agent instance. Sticks to the
// top and collapses into a compact strip once scrolled past, so it doesn't
// keep eating space above the deploy picker.
//
// Collapse is a one-way latch driven by scroll, NOT a two-way toggle: once
// collapsed, only an explicit click on the compact strip re-expands it. A
// scroll-driven two-way toggle here is a feedback loop waiting to happen —
// if collapsing ever removes more height than this page's own overflow,
// the browser clamps scrollTop back to 0, and a handler that reacts to
// *that* by re-expanding immediately undoes the collapse, which restores
// the overflow, which the next frame reads as "still scrolled", collapsing
// it again — an infinite bounce that nets to no visible change at all.
import { useState, type UIEvent } from "react";
import { Info } from "lucide-react";

import { DeployAgentPanel } from "../demo-apps/DeployAgentPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { ReviewContextComparison } from "./ReviewContextComparison";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";

const COLLAPSE_THRESHOLD_PX = 32;

export function DeployAgentStage({
  demoApp,
  session,
  onGoToCreateDatapack,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  const [comparisonCollapsed, setComparisonCollapsed] = useState(false);

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!demoApp.deployable) {
    return <p className="p-6 text-sm text-muted-foreground">This lab has nothing to deploy.</p>;
  }

  const handleScroll = (e: UIEvent<HTMLDivElement>) => {
    if (e.currentTarget.scrollTop > COLLAPSE_THRESHOLD_PX) {
      setComparisonCollapsed(true);
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto" onScroll={handleScroll}>
      <div className="sticky top-0 z-10 bg-background px-4 pt-4">
        {comparisonCollapsed ? (
          <button
            type="button"
            onClick={() => setComparisonCollapsed(false)}
            className="flex w-full items-center gap-2 rounded-md border border-border px-3 py-2 text-left text-sm text-muted-foreground hover:bg-muted/50"
          >
            <Info className="h-4 w-4 shrink-0" aria-hidden />
            What context changes for {demoApp.agentDisplayName}'s reviews — click to show again.
          </button>
        ) : (
          <ReviewContextComparison agentDisplayName={demoApp.agentDisplayName} />
        )}
      </div>
      <div className="flex flex-col gap-4 p-4 pt-4">
        <DeployAgentPanel
          agentSlug={demoApp.deployable.agentSlug}
          agentDisplayName={demoApp.agentDisplayName}
          sourceRepoUrl={demoApp.deployable.sourceRepoUrl}
          session={session}
        />
      </div>
    </div>
  );
}
