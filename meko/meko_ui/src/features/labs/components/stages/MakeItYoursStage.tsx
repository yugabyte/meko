// Closes the lab: point AgentK at a real pull request of the user's own,
// then what they actually built (real counts, not a script) and the ways to
// take the same setup back to their work. Absorbed the old standalone
// "Review a real PR" step, which was the same screen split in two.

import { CheckCircle2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { StatTile } from "@/components/ui/stat-tile";
import { cn } from "@/lib/utils";
import { formatTokens } from "@/lib/tokenSavings";

import { useLabReviewDashboard } from "../../hooks/useLabReviewDashboard";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { LabDemoAppConfig } from "../../types";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import type { ReviewAgentConfig } from "../demo-apps/ReviewAgentConfigPanel";
import { BringYourOwnPrStage } from "./BringYourOwnPrStage";
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
  demoApp,
  session,
  sessionRuns,
  reviewConfig,
  byocRun,
  onByocRunChange,
  onGoToCreateDatapack,
  onGoToDeployAgent,
  onGoToCollaborate,
  onEndLab,
}: {
  demoApp: LabDemoAppConfig;
  session: ReturnType<typeof useLabRunnerSession>;
  sessionRuns: PrReviewRunState[];
  reviewConfig: ReviewAgentConfig;
  byocRun: PrReviewRunState | null;
  onByocRunChange: (run: PrReviewRunState) => void;
  onGoToCreateDatapack: () => void;
  onGoToDeployAgent: () => void;
  onGoToCollaborate: () => void;
  onEndLab: () => void;
}) {
  const { totals, savings } = useLabReviewDashboard(session.datapackId);
  const reviewsRun = sessionRuns.filter((run) => run.reviewUsage).length;

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 5 · Step 3"
        title="Make it yours"
        lede={`Every review so far used a bundled sample. Point ${demoApp.agentDisplayName} at a pull request of your own, reviewed against the same context.`}
        tags={["Your repo"]}
      />

      {/* The BYO-PR flow, hosted rather than given its own step: it is the
          same screen as the summary below, and splitting them made the user
          leave the review to read what they had built.
          Before a run there is no wrapper at all -- the fetch form carries its
          own border, and boxing it produced a second frame the form was then
          clipped inside, because a `min-h-0` child of this scrolling column
          shrinks below its content. After a run the transcript is what needs
          bounding, so the frame and the cap appear then and scroll internally
          rather than pushing the summary off the page. */}
      <div
        className={cn(
          "flex flex-col",
          byocRun && "max-h-[32rem] min-h-0 overflow-hidden rounded-lg border border-border",
        )}
      >
        <BringYourOwnPrStage
          demoApp={demoApp}
          session={session}
          reviewConfig={reviewConfig}
          savedRun={byocRun}
          onRunChange={onByocRunChange}
          onGoToCreateDatapack={onGoToCreateDatapack}
          onGoToDeployAgent={onGoToDeployAgent}
        />
      </div>

      <div>
        <h3 className="text-sm font-semibold text-foreground">You built the loop</h3>
        <p className="mt-1 text-xs text-muted-foreground">
          Everything below is {session.datapackName ?? "your datapack"}'s own record. Nothing in
          this lab was a simulation of a product that doesn't exist.
        </p>
      </div>

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
