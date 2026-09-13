// A personal dashboard of this datapack's review runs -- stat tiles plus a
// runs-over-time chart, modeled at reduced scope on the user's own internal
// AgentK ops dashboard (a single-user view of their own runs, not that
// dashboard's bot-uptime/watched-repos/multi-repo widgets, which don't map
// to one lab's single datapack).
//
// NOT WIRED UP YET: there is no backend path today for writing a review run
// into the datapack's AMP cluster or reading it back -- see
// chapter3-plan-a-meko.md (api_server + meko_ui write/read endpoints) and
// chapter3-plan-b-amp-controller.md (the AMP-controller SQL-execution
// primitive both depend on), both uploaded to the meko-labs-chapter3
// datapack. Until that lands, this renders SAMPLE_REVIEW_RUNS -- clearly
// labeled sample data, never a live query -- so the dashboard's shape is
// real even though its numbers aren't yet.

import { AlertTriangle, BarChart3, Coins, ListChecks, Sparkles } from "lucide-react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import { StatTile } from "@/components/ui/stat-tile";

import { LabStageNeedsAmpCluster } from "../lab-runner/LabStageNeedsAmpCluster";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { SAMPLE_REVIEW_RUNS } from "../../data/adlcReviewAgentLab/sampleReviewRuns";
import { useValidAmpCluster } from "../../hooks/useValidAmpCluster";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";

function formatUsd(amount: number): string {
  return `$${amount.toFixed(4)}`;
}

function formatDate(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function ReviewDashboardStage({
  session,
  onGoToCreateDatapack,
  onGoToCreateAmpCluster,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
  onGoToCreateAmpCluster: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  return (
    <ReviewDashboardStageContent
      datapackId={session.datapackId}
      session={session}
      onGoToCreateAmpCluster={onGoToCreateAmpCluster}
    />
  );
}

function ReviewDashboardStageContent({
  datapackId,
  session,
  onGoToCreateAmpCluster,
}: {
  datapackId: string;
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateAmpCluster: () => void;
}) {
  // Confirms session.ampClusterId still exists (see useValidAmpCluster's own
  // comment) before trusting it -- otherwise a cluster deleted from the
  // datapack detail page would still pass this gate and render a dashboard
  // for a cluster that's gone.
  useValidAmpCluster(datapackId, session);
  if (!session.ampClusterId) {
    return <LabStageNeedsAmpCluster onGoToCreateAmpCluster={onGoToCreateAmpCluster} />;
  }

  const runs = SAMPLE_REVIEW_RUNS;
  const totalReviews = runs.length;
  const totalInputTokens = runs.reduce((sum, r) => sum + r.inputTokens, 0);
  const totalOutputTokens = runs.reduce((sum, r) => sum + r.outputTokens, 0);
  const totalCostUsd = runs.reduce((sum, r) => sum + r.estimatedCostUsd, 0);
  const avgFindings = totalReviews > 0 ? runs.reduce((sum, r) => sum + r.findingsCount, 0) / totalReviews : 0;

  const chartData = runs.map((r) => ({
    date: formatDate(r.createdAt),
    costUsd: r.estimatedCostUsd,
  }));

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-4">
      <div className="flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <BarChart3 className="h-5 w-5 text-primary" aria-hidden />
          <h2 className="text-lg font-semibold text-foreground">Your review dashboard</h2>
        </div>
        <span className="inline-flex items-center gap-1.5 rounded-full border border-amber-500/30 bg-amber-500/10 px-2.5 py-1 text-xs font-medium text-amber-700 dark:text-amber-300">
          <Sparkles className="h-3 w-3" aria-hidden />
          Sample data — live capture coming soon
        </span>
      </div>
      <p className="text-sm text-muted-foreground">
        This is what your dashboard will look like once review runs are recorded into your
        AMP cluster — the numbers below are illustrative, not yours yet.
      </p>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          icon={<ListChecks className="h-3.5 w-3.5" aria-hidden />}
          label="Reviews run"
          value={totalReviews}
        />
        <StatTile
          icon={<Coins className="h-3.5 w-3.5" aria-hidden />}
          label="Total tokens"
          value={`${(totalInputTokens / 1000).toFixed(1)}k in / ${(totalOutputTokens / 1000).toFixed(1)}k out`}
        />
        <StatTile
          icon={<Coins className="h-3.5 w-3.5" aria-hidden />}
          label="Total estimated cost"
          value={formatUsd(totalCostUsd)}
        />
        <StatTile
          icon={<AlertTriangle className="h-3.5 w-3.5" aria-hidden />}
          label="Avg findings / review"
          value={avgFindings.toFixed(1)}
        />
      </div>

      <div className="rounded-xl border border-border bg-card p-4">
        <p className="mb-3 text-sm font-medium text-foreground">Estimated cost per review</p>
        <ResponsiveContainer width="100%" height={220}>
          <LineChart data={chartData} margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
            <CartesianGrid strokeDasharray="3 3" className="stroke-border" />
            <XAxis dataKey="date" tick={{ fontSize: 11 }} className="text-muted-foreground" />
            <YAxis
              tickFormatter={(v: number) => `$${v.toFixed(3)}`}
              tick={{ fontSize: 11 }}
              className="text-muted-foreground"
              width={56}
            />
            <Tooltip
              formatter={(value: number) => formatUsd(value)}
              contentStyle={{
                backgroundColor: "hsl(var(--card))",
                border: "1px solid hsl(var(--border))",
                borderRadius: "6px",
                fontSize: "12px",
              }}
            />
            <Line
              type="monotone"
              dataKey="costUsd"
              name="Estimated cost"
              stroke="hsl(25 95% 53%)"
              strokeWidth={2}
              dot={{ r: 3 }}
            />
          </LineChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}
