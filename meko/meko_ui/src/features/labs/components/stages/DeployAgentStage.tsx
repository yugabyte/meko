// Stage 3: standalone now — no longer gated behind a completed review.
// DeployAgentPanel itself is unchanged; only its reachability changed.
// Leads with ReviewLoopDiagram so deploying isn't a bare picker — it
// explains what the agent you're about to deploy actually does. The
// diagram sticks to the top and collapses into a compact strip once
// scrolled past, so it doesn't keep eating space above the deploy picker.
//
// Collapse is a one-way latch driven by scroll, NOT a two-way toggle: once
// collapsed, only an explicit click on the compact strip re-expands it.
// A scroll-driven two-way toggle here is a feedback loop waiting to
// happen — collapsing removes far more height than this page's own
// overflow (diagram ~300px vs ~130px of actual scrollable content), so the
// instant it collapses, scrollHeight drops below clientHeight and the
// browser is forced to clamp scrollTop back to 0. A handler that reacts to
// *that* scroll event by re-expanding (because scrollTop is back near 0)
// immediately undoes the collapse, which restores the overflow, which the
// user's next frame reads as "still scrolled", collapsing it again — an
// infinite bounce that nets to no visible change at all.
import { useState, type UIEvent } from "react";
import { Info } from "lucide-react";

import { DeployAgentPanel } from "../demo-apps/DeployAgentPanel";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { ReviewLoopDiagram } from "./ReviewLoopDiagram";
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
  const [diagramCollapsed, setDiagramCollapsed] = useState(false);

  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!demoApp.deployable) {
    return <p className="p-6 text-sm text-muted-foreground">This lab has nothing to deploy.</p>;
  }

  const handleScroll = (e: UIEvent<HTMLDivElement>) => {
    if (e.currentTarget.scrollTop > COLLAPSE_THRESHOLD_PX) {
      setDiagramCollapsed(true);
    }
  };

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto" onScroll={handleScroll}>
      <div className="sticky top-0 z-10 bg-background px-4 pt-4">
        {diagramCollapsed ? (
          <button
            type="button"
            onClick={() => setDiagramCollapsed(false)}
            className="flex w-full items-center gap-2 rounded-md border border-border px-3 py-2 text-left text-sm text-muted-foreground hover:bg-muted/50"
          >
            <Info className="h-4 w-4 shrink-0" aria-hidden />
            How {demoApp.agentDisplayName} reads your datapack — click to show the diagram again.
          </button>
        ) : (
          <ReviewLoopDiagram agentDisplayName={demoApp.agentDisplayName} />
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
