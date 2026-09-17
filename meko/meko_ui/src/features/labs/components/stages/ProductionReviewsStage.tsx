// Chapter 5's opener: the same loop the user just built, running against
// Yugabyte's own 104 repositories. Every figure is a dated snapshot, never a
// live poll -- see productionSnapshot.ts.

import { Boxes, Coins, ExternalLink, FileText, Layers, Sparkles, ThumbsUp } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { StatTile } from "@/components/ui/stat-tile";

import {
  PRODUCTION_DASHBOARD_URL,
  PRODUCTION_SNAPSHOT,
  PRODUCTION_SNAPSHOT_READ_ON,
} from "../../data/adlcReviewAgentLab/productionSnapshot";
import { LabStageHeader } from "./LabStageHeader";

const P = PRODUCTION_SNAPSHOT;

export function ProductionReviewsStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 5 · Step 1"
        title="This isn't a demo bot"
        lede={`Everything you just did is how Yugabyte reviews its own code. AgentK watches ${P.reposWatched} repositories and reviews every pull request onto a default branch. Yugabyte is an open-source company, so a good share of those reviews are sitting in public on GitHub.`}
        tags={["Production"]}
      />

      <div className="flex flex-wrap items-center gap-2">
        <Badge variant="secondary" className="text-[10px]">
          read {PRODUCTION_SNAPSHOT_READ_ON}
        </Badge>
        <span className="text-xs text-muted-foreground">
          A snapshot, not a live feed. The dashboard has moved on since.
        </span>
        <a
          href={PRODUCTION_DASHBOARD_URL}
          target="_blank"
          rel="noreferrer noopener"
          className="ml-auto inline-flex items-center gap-1 text-xs text-primary hover:underline"
        >
          <ExternalLink className="h-3 w-3" aria-hidden />
          Open the dashboard
        </a>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <StatTile
          icon={<ThumbsUp className="h-3 w-3" />}
          label="Issues found"
          value={P.issuesFound.toLocaleString()}
          hint={`${P.runs.toLocaleString()} runs · ${P.prs.toLocaleString()} PRs`}
        />
        <StatTile
          icon={<Coins className="h-3 w-3" />}
          label="Cost savings"
          value={`$${P.costSavingsUsd.toLocaleString()}`}
          hint={`$${P.costPerPrUsd} per PR · $${P.totalSpendUsd.toLocaleString()} spent`}
        />
        <StatTile
          icon={<FileText className="h-3 w-3" />}
          label="Guidelines in context"
          value={P.guidelinesInContext.toLocaleString()}
          hint="review rules in the Meko corpus"
        />
        <StatTile
          icon={<Sparkles className="h-3 w-3" />}
          label="Learnings applied"
          value={P.learningsApplied.toLocaleString()}
          hint="credited in posted comments"
        />
        <StatTile
          icon={<Layers className="h-3 w-3" />}
          label="Tokens, 30 days"
          value={P.tokens30d}
          hint={`${P.tokensIn30d} in · ${P.tokensOut30d} out`}
        />
        <StatTile
          icon={<Boxes className="h-3 w-3" />}
          label="Repos watched"
          value={P.reposWatched.toLocaleString()}
          hint="all auto-review new PRs"
        />
      </div>

      <section className="flex flex-col gap-2 rounded-lg border border-border bg-card p-4">
        <p className="text-sm font-medium text-foreground">Reviewer verdicts</p>
        <p className="text-xs text-muted-foreground">
          <span className="text-context-knowledge">{P.verdictsUp.toLocaleString()} up</span> ·{" "}
          {P.verdictsDown} down · {P.verdictsUpPct}% of labelled comments judged right by the
          engineer who received them.
        </p>
      </section>

      <section className="flex flex-col gap-1 rounded-lg border border-primary/40 bg-primary/[0.03] p-4">
        <p className="text-sm font-medium text-foreground">The same loop, at scale</p>
        <p className="text-xs text-muted-foreground">
          The {P.guidelinesInContext.toLocaleString()} guidelines are the knowledge base you loaded
          in Chapter 1. The {P.learningsApplied} learnings applied are collective memory doing what
          you did in Chapter 4. Nothing here is a mechanism you haven't already used.
        </p>
      </section>
    </div>
  );
}
