// What this datapack has accumulated, read back from Meko's own records:
// conversations, extracted memories, requests and tokens saved, activity over
// time, and what each model saved. Nothing here is per-run, so it holds
// across reloads in a way a single review's numbers don't.

import { Coins, Layers, MessageSquare, Zap } from "lucide-react";
import { CartesianGrid, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";

import { StatTile } from "@/components/ui/stat-tile";
import { dollarsSavedByModel, formatTokens } from "@/lib/tokenSavings";

import { useLabReviewDashboard } from "../../hooks/useLabReviewDashboard";

function formatDay(iso: string): string {
  return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

export function DatapackActivitySummary({ datapackId }: { datapackId: string | null }) {
  const { totals, activity, savings, error } = useLabReviewDashboard(datapackId);

  const byModel = savings ? dollarsSavedByModel(savings) : [];
  const chartData = activity.map((point) => ({ ...point, label: formatDay(point.date) }));

  return (
    <div className="flex flex-col gap-6">
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
              <YAxis allowDecimals={false} tick={{ fontSize: 11 }} className="text-muted-foreground" width={32} />
              <Tooltip
                contentStyle={{
                  backgroundColor: "hsl(var(--card))",
                  border: "1px solid hsl(var(--border))",
                  borderRadius: "6px",
                  fontSize: "12px",
                }}
              />
              {/* Semantic token, not a literal: the line has to stay legible
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

      {byModel.length > 0 && (
        <section className="flex flex-col gap-2">
          <h3 className="text-sm font-medium text-foreground">What this datapack saved, by model</h3>
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
