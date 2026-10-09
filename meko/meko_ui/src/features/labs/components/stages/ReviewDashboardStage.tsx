// A dashboard of this datapack's own activity, drawn entirely from data Meko
// already has: the conversation history every review writes into, and the
// datapack's token-savings summary. Per-run tokens/cost/findings still have
// nowhere to persist, so those come from this session's own runs and are
// labelled as such rather than faked for earlier ones.

import { Coins, Layers, MessageSquare, Zap } from "lucide-react";
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
import { dollarsSavedByModel, formatTokens } from "@/lib/tokenSavings";

import { useLabReviewDashboard } from "../../hooks/useLabReviewDashboard";
// PARKED with the create-amp-cluster stage, see chapters.ts.
// import { useValidAmpCluster } from "../../hooks/useValidAmpCluster";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
// import { LabStageNeedsAmpCluster } from "../lab-runner/LabStageNeedsAmpCluster";
import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { LabStageHeader } from "./LabStageHeader";
import { SessionRunsTable } from "./SessionRunsTable";

function formatDay(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function ReviewDashboardStage({
  session,
  sessionRuns,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  /** The runs LabStageSlot is holding for this lab run — the only source of
   *  real per-review tokens/cost/findings that exists today. */
  sessionRuns: PrReviewRunState[];
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  return <ReviewDashboardStageContent datapackId={session.datapackId} sessionRuns={sessionRuns} />;
}

function ReviewDashboardStageContent({
  datapackId,
  sessionRuns,
}: {
  datapackId: string;
  sessionRuns: PrReviewRunState[];
}) {
  // No AMP gate: everything below is read out of Meko, so a cluster was a
  // prerequisite this page never used. Parked with that stage, see chapters.ts.
  const { totals, activity, savings, error } = useLabReviewDashboard(datapackId);

  const byModel = savings ? dollarsSavedByModel(savings) : [];
  const chartData = activity.map((point) => ({ ...point, label: formatDay(point.date) }));

  return (
    <div className="flex flex-1 flex-col gap-6 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 3"
        title="Your datapack's dashboard"
        lede="Every number here is this datapack's own, read back from the conversations and token accounting Meko already keeps."
        tags={["Live"]}
      />

      {error && <p className="text-sm text-muted-foreground">{error}</p>}

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          icon={<MessageSquare className="h-3 w-3" />}
          label="Conversations"
          value={totals.conversations.toLocaleString()}
        />
        <StatTile
          icon={<Layers className="h-3 w-3" />}
          label="Memories extracted"
          value={totals.memoriesAdded.toLocaleString()}
        />
        <StatTile
          icon={<Zap className="h-3 w-3" />}
          label="Requests through Meko"
          value={(savings?.total_requests ?? 0).toLocaleString()}
        />
        <StatTile
          icon={<Coins className="h-3 w-3" />}
          label="Tokens saved"
          value={formatTokens(savings?.total_tokens_saved ?? 0)}
        />
      </div>

      {chartData.length > 0 && (
        <section className="rounded-xl border border-border bg-card p-4">
          <h3 className="mb-3 text-sm font-medium text-foreground">Activity over time</h3>
          <ResponsiveContainer width="100%" height={220}>
            <LineChart data={chartData} margin={{ top: 4, right: 16, left: 8, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" className="stroke-border" />
              <XAxis dataKey="label" tick={{ fontSize: 11 }} className="text-muted-foreground" />
              <YAxis
                allowDecimals={false}
                tick={{ fontSize: 11 }}
                className="text-muted-foreground"
                width={32}
              />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  fontSize: "12px",
                }}
              />
              {/* Semantic token, not a literal — the line has to stay legible
                  when the viewer flips theme. */}
              <Line
                type="monotone"
                dataKey="conversations"
                name="Conversations"
                stroke="hsl(var(--primary))"
                strokeWidth={2}
                dot={{ r: 3 }}
              />
            </LineChart>
          </ResponsiveContainer>
        </section>
      )}

      <section className="flex flex-col gap-2">
        <h3 className="text-sm font-medium text-foreground">
          Reviews in this session
          <span className="ml-2 text-xs font-normal text-muted-foreground">
            · per-run numbers aren't persisted yet, so these reset on reload
          </span>
        </h3>
        <SessionRunsTable runs={sessionRuns} />
      </section>

      {byModel.length > 0 && (
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-medium text-foreground">
            What this datapack saved, by model
          </h3>
          <div className="grid gap-3 sm:grid-cols-3">
            {byModel.map((entry) => (
              <StatTile
                key={entry.key}
                icon={<Coins className="h-3 w-3" />}
                label={entry.label}
                value={`$${entry.dollars.toFixed(2)}`}
              />
            ))}
          </div>
        </section>
      )}
    </div>
  );
}
