// Closes the lab: what the user actually built (real counts, not a script),
// then the four ways to point the same setup at their own work, and the MCP
// config that connects their own tools to this datapack.

import { CheckCircle2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { StatTile } from "@/components/ui/stat-tile";
import { formatTokens } from "@/lib/tokenSavings";

import { useLabReviewDashboard } from "../../hooks/useLabReviewDashboard";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { LabStageHeader } from "./LabStageHeader";

/** The four directions out of the lab. Each is a real product surface the
 *  user already has access to — no coming-soon tiles. */
const NEXT_STEPS = [
  {
    title: "Bring your own standards",
    body: "Upload your team's real coding documents instead of the sample excerpt.",
  },
  {
    title: "Review your own pull requests",
    body: "Point AgentK at a live PR with a GitHub token that never leaves your browser.",
  },
  {
    title: "Deploy your own AgentK",
    body: "Fork the agent and run your own dedicated instance on AgentCore.",
  },
  {
    title: "Invite your team",
    body: "Share this datapack so everyone's agents read the same rules.",
  },
];

export function MakeItYoursStage({
  session,
  sessionRuns,
  onGoToCollaborate,
  onEndLab,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  sessionRuns: PrReviewRunState[];
  onGoToCollaborate: () => void;
  onEndLab: () => void;
}) {
  const { totals, savings } = useLabReviewDashboard(session.datapackId);
  const reviewsRun = sessionRuns.filter((run) => run.reviewUsage).length;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Lab complete"
        title="You built the loop"
        lede={`Everything below is ${session.datapackName ?? "your datapack"}'s own record. Nothing in this lab was a simulation of a product that doesn't exist.`}
      />

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          icon={<CheckCircle2 className="h-3 w-3" />}
          label="Reviews this session"
          value={reviewsRun.toLocaleString()}
        />
        <StatTile
          icon={<CheckCircle2 className="h-3 w-3" />}
          label="Conversations"
          value={totals.conversations.toLocaleString()}
        />
        <StatTile
          icon={<CheckCircle2 className="h-3 w-3" />}
          label="Memories extracted"
          value={totals.memoriesAdded.toLocaleString()}
        />
        <StatTile
          icon={<CheckCircle2 className="h-3 w-3" />}
          label="Tokens saved"
          value={formatTokens(savings?.total_tokens_saved ?? 0)}
        />
      </div>

      <section className="flex flex-col gap-3">
        <h3 className="text-sm font-semibold text-foreground">Point it at your own work</h3>
        <div className="grid gap-3 sm:grid-cols-2">
          {NEXT_STEPS.map(({ title, body }) => (
            <div key={title} className="rounded-lg border border-border bg-card p-4">
              <p className="text-sm font-semibold text-foreground">{title}</p>
              <p className="mt-1 text-xs text-muted-foreground">{body}</p>
            </div>
          ))}
        </div>
      </section>

      <div className="flex flex-wrap gap-3">
        <Button onClick={onGoToCollaborate}>Share this datapack</Button>
        <Button variant="outline" onClick={onEndLab}>
          Back to Labs
        </Button>
      </div>
    </div>
  );
}
