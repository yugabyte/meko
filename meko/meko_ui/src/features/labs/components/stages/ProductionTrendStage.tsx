// Chapter 5's second step: the shape of a month of production reviews. The
// point of the chart is the trend, not any single day -- the line climbs
// because teams keep adding repos.

import { Activity, Coins, Flame, Play } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { StatTile } from "@/components/ui/stat-tile";

import {
  PRODUCTION_AVG_RUNS_PER_DAY,
  PRODUCTION_BUSIEST_DAY,
  PRODUCTION_RUNS_30D_TOTAL,
  PRODUCTION_SNAPSHOT,
  PRODUCTION_SNAPSHOT_READ_ON,
} from "../../data/adlcReviewAgentLab/productionSnapshot";
import { RunsPerDayChart } from "../demo-apps/RunsPerDayChart";
import { LabStageHeader } from "./LabStageHeader";

const P = PRODUCTION_SNAPSHOT;

export function ProductionTrendStage() {
  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 5 · Step 2"
        title="Thirty days of reviews"
        lede="Every bar is a day of AgentK reviewing pull requests nobody asked a human to look at first. The line climbs because teams keep adding repositories, and because the reviews were worth reading."
        tags={["Production"]}
      />

      <section className="flex flex-col gap-3 rounded-lg border border-border bg-card p-4">
        <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
          <p className="text-sm font-medium text-foreground">Runs per day</p>
          <p className="text-xs text-muted-foreground">
            {PRODUCTION_AVG_RUNS_PER_DAY} runs on an average day in this window
          </p>
          <Badge variant="secondary" className="ml-auto text-[10px]">
            read {PRODUCTION_SNAPSHOT_READ_ON}
          </Badge>
        </div>
        <RunsPerDayChart />
      </section>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
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
        <StatTile
          icon={<Play className="h-3 w-3" />}
          label="Runs reviewed"
          value={P.runs.toLocaleString()}
          hint={`${P.prs.toLocaleString()} pull requests`}
        />
      </div>

      <section className="flex flex-col gap-1 rounded-lg border border-primary/40 bg-primary/[0.03] p-4">
        <p className="text-sm font-medium text-foreground">Why it reviews everything</p>
        <p className="text-xs text-muted-foreground">
          At ${P.costPerPrUsd} a pull request against the cost of a reviewer-hour, the savings pay
          for the system several times over. That is what makes reviewing every PR affordable
          instead of sampling.
        </p>
      </section>
    </div>
  );
}
