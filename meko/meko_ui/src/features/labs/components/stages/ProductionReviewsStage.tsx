// Chapter 4's closing numbers: a month of runs and the dashboard totals
// behind them, in one lesson.
//
// These were two steps. Splitting them put a 30-day chart on one screen and
// the totals that chart adds up to on the next, so a reader had to hold the
// first to make sense of the second. Every figure is a dated snapshot, never
// a live poll -- see productionSnapshot.ts.

import {
  Activity,
  Boxes,
  Coins,
  ExternalLink,
  FileText,
  Flame,
  Layers,
  Sparkles,
  ThumbsUp,
} from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { StatTile } from "@/components/ui/stat-tile";

import {
  PRODUCTION_AVG_RUNS_PER_DAY,
  PRODUCTION_BUSIEST_DAY,
  PRODUCTION_DASHBOARD_URL,
  PRODUCTION_RUNS_30D_TOTAL,
  PRODUCTION_SNAPSHOT,
  PRODUCTION_SNAPSHOT_READ_ON,
} from "../../data/adlcReviewAgentLab/productionSnapshot";
import { RunsPerDayChart } from "../demo-apps/RunsPerDayChart";
import { LabStageHeader } from "./LabStageHeader";

const P = PRODUCTION_SNAPSHOT;

export function ProductionReviewsStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 4"
        title="Thirty days of runs"
        lede={`The threads you just read are a handful out of this. AgentK watches ${P.reposWatched} repositories and reviews every pull request onto a default branch, and here is what a month of that looks like.`}
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

      <section className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <p className="text-sm font-medium text-foreground">Runs per day</p>
          <p className="text-xs text-muted-foreground">
            {PRODUCTION_AVG_RUNS_PER_DAY} runs on an average day in this window
          </p>
        </div>
        <RunsPerDayChart />
        <div className="grid gap-3 border-t border-border pt-3 sm:grid-cols-3">
          <StatTile
            icon={<Activity className="h-3 w-3" />}
            label="Reviews in 30 days"
            value={PRODUCTION_RUNS_30D_TOTAL.toLocaleString()}
          />
          <StatTile
            icon={<Flame className="h-3 w-3" />}
            label="Busiest day"
            value={PRODUCTION_BUSIEST_DAY.runs.toLocaleString()}
            hint={PRODUCTION_BUSIEST_DAY.day}
          />
          <StatTile
            icon={<Coins className="h-3 w-3" />}
            label="Average cost per PR"
            value={`$${P.costPerPrUsd}`}
          />
        </div>
      </section>

      <h3 className="text-sm font-semibold text-foreground">All time, from its dashboard</h3>

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
        <p className="text-sm font-medium text-foreground">Why it reviews everything</p>
        <p className="text-xs text-muted-foreground">
          The {P.guidelinesInContext.toLocaleString()} guidelines are the knowledge base you loaded
          in Chapter 1, and the {P.learningsApplied} learnings applied are findings written back the
          way you wrote yours back in Chapter 3. At ${P.costPerPrUsd} a pull request against the
          cost of a reviewer-hour, that context is what makes reviewing every one of them affordable
          instead of sampling. Nothing here is a mechanism you haven't already used.
        </p>
      </section>
    </div>
  );
}
