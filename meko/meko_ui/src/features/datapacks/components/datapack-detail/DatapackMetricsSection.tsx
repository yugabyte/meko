import { useState } from "react";
import { cn } from "@/lib/utils";
import { RefreshCw } from "lucide-react";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";

type Props = {
  datapackId: string | null;
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
  learningsCount: number;
};

function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`;
  return String(n);
}

/** Compact 3-chip summary row for the Overview tab (M01, M02, M03). */
export function DatapackMetricsSummaryRow({
  datapackId,
  knowledgeCount,
  memoryCount,
  collectiveMemoryCount,
  onNavigateToMetric,
}: {
  datapackId: string | null;
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
  onNavigateToMetric?: (sectionId: string) => void;
}) {
  const { data: metrics } = useDatapackMetrics(datapackId);

  const kbTokens = knowledgeCount * 1000;
  const privateMemTokens = memoryCount * 200;
  const promotedMemTokens = collectiveMemoryCount * 200;
  const totalTokens = kbTokens + privateMemTokens + promotedMemTokens;

  const m01Label = totalTokens === 0
    ? "0 ctx"
    : totalTokens >= 1_000_000
      ? `${(totalTokens / 1_000_000).toFixed(1)}M ctx`
      : totalTokens >= 1_000
        ? `${Math.round(totalTokens / 1_000)}K ctx`
        : `${totalTokens} ctx`;

  const scoreTotal =
    (metrics?.score_high_total ?? 0) +
    (metrics?.score_mid_total ?? 0) +
    (metrics?.score_low_total ?? 0);
  const qualityScore = scoreTotal > 0 ? (metrics?.score_high_total ?? 0) / scoreTotal : null;

  const m03Label = qualityScore === null ? null
    : qualityScore > 0.75 ? "High quality"
    : qualityScore > 0.5 ? "Mid quality"
    : "Low quality";
  const m03Color = qualityScore === null ? ""
    : qualityScore > 0.75 ? "text-green-600 dark:text-green-400"
    : qualityScore > 0.5 ? "text-amber-500 dark:text-amber-400"
    : "text-red-600 dark:text-red-400";

  const hasSavings = metrics && metrics.total_requests > 0;
  const savingsPct = hasSavings ? `${(metrics!.savings_pct * 100).toFixed(0)}% saved` : null;

  const chipCls = cn(
    "flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 text-left transition-colors",
    onNavigateToMetric
      ? "cursor-pointer hover:border-primary/40 hover:bg-muted/40 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary"
      : "cursor-default",
  );

  return (
    <div className="flex flex-wrap gap-2">
      {/* M01 */}
      <button
        type="button"
        className={chipCls}
        onClick={() => onNavigateToMetric?.("metric-context-size")}
      >
        <span className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">Context</span>
        <span className="text-sm font-semibold tabular-nums text-foreground">{m01Label}</span>
      </button>

      {/* M02 */}
      <button
        type="button"
        className={chipCls}
        onClick={() => onNavigateToMetric?.("metric-token-savings")}
      >
        <span className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">Savings</span>
        {hasSavings ? (
          <span className="text-sm font-semibold tabular-nums text-green-600 dark:text-green-400">{savingsPct}</span>
        ) : (
          <span className="text-sm font-semibold text-muted-foreground/50">—</span>
        )}
      </button>

      {/* M03 */}
      <button
        type="button"
        className={chipCls}
        onClick={() => onNavigateToMetric?.("metric-match-quality")}
      >
        <span className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">Quality</span>
        {m03Label ? (
          <span className={cn("text-sm font-semibold", m03Color)}>{m03Label}</span>
        ) : (
          <span className="text-sm font-semibold text-muted-foreground/50">—</span>
        )}
      </button>

      {/* Queries */}
      {metrics && (
        <button
          type="button"
          className={chipCls}
          onClick={() => onNavigateToMetric?.("metric-query-activity")}
        >
          <span className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">Queries</span>
          <span className="text-sm font-semibold tabular-nums text-foreground">
            {(metrics.daily_activity ?? []).reduce((s, d) => s + d.hybrid + d.kb + d.memory, 0)}
          </span>
        </button>
      )}
    </div>
  );
}

export function DatapackMetricsSection({
  datapackId,
  knowledgeCount,
  memoryCount,
  collectiveMemoryCount,
  learningsCount,
}: Props) {
  const { data: metrics, isLoading: metricsLoading, isFetching, refetch } = useDatapackMetrics(datapackId);
  const nowMs = useNowMs();

  const kbTokens = knowledgeCount * 1000;
  const privateMemTokens = memoryCount * 200;
  const promotedMemTokens = collectiveMemoryCount * 200;
  const totalTokens = kbTokens + privateMemTokens + promotedMemTokens;

  const kbPct = totalTokens > 0 ? (kbTokens / totalTokens) * 100 : 0;
  const promotedPct = totalTokens > 0 ? (promotedMemTokens / totalTokens) * 100 : 0;
  const privatePct = totalTokens > 0 ? (privateMemTokens / totalTokens) * 100 : 0;

  const privateMemCount = Math.max(0, memoryCount - collectiveMemoryCount);
  const pendingCount = learningsCount;

  // M-03 totals
  const scoreTotal =
    (metrics?.score_high_total ?? 0) +
    (metrics?.score_mid_total ?? 0) +
    (metrics?.score_low_total ?? 0);
  const highPct = scoreTotal > 0 ? ((metrics?.score_high_total ?? 0) / scoreTotal) * 100 : 0;
  const midPct = scoreTotal > 0 ? ((metrics?.score_mid_total ?? 0) / scoreTotal) * 100 : 0;
  const lowPct = scoreTotal > 0 ? ((metrics?.score_low_total ?? 0) / scoreTotal) * 100 : 0;

  // M-07 quality score
  const qualityScore = scoreTotal > 0 ? (metrics?.score_high_total ?? 0) / scoreTotal : 0;
  const qualityBanner =
    qualityScore > 0.75
      ? { label: "Strong retrieval", color: "bg-green-500/10 border-green-500/20 text-green-700 dark:text-green-300", dot: "bg-green-500" }
      : qualityScore > 0.5
        ? { label: "Moderate", color: "bg-amber-500/10 border-amber-500/20 text-amber-700 dark:text-amber-300", dot: "bg-amber-400" }
        : { label: "Needs attention", color: "bg-red-500/10 border-red-500/20 text-red-700 dark:text-red-300", dot: "bg-red-500" };

  // M-06 daily activity
  const activity = metrics?.daily_activity ?? [];
  const totalQueryCount = activity.reduce((s, d) => s + d.hybrid + d.kb + d.memory, 0);

  return (
    <div>
      {/* Meko Context */}
      <div className="flex items-center gap-3 mb-4">
        <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          Meko Context
        </span>
        <div className="flex-1 h-px bg-border" />
        <button
          onClick={() => refetch()}
          disabled={isFetching}
          className="flex items-center gap-1.5 text-xs text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          title="Refresh metrics"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
          Refresh
        </button>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {/* M-01 Total Context Size */}
        <div id="metric-context-size" className="rounded-xl border border-border bg-card p-4 space-y-3">
          <div>
            <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground mb-1">
              Total Context Size
            </p>
            <p className="text-2xl font-semibold tabular-nums text-foreground">
              {totalTokens === 0 ? "—" : formatTokens(totalTokens)}
              {totalTokens > 0 && (
                <span className="text-sm font-normal text-muted-foreground ml-1">tokens</span>
              )}
            </p>
          </div>

          {totalTokens > 0 && (
            <>
              <div className="flex h-2 w-full overflow-hidden rounded-full bg-muted">
                <div className="bg-blue-500" style={{ width: `${kbPct}%` }} />
                <div className="bg-teal-500" style={{ width: `${promotedPct}%` }} />
                <div className="bg-purple-500" style={{ width: `${privatePct}%` }} />
              </div>

              <div className="space-y-1">
                <LegendRow color="bg-blue-500" label="Knowledge" count={knowledgeCount} pct={kbPct} unit="chunks" />
                <LegendRow color="bg-teal-500" label="Promoted" count={collectiveMemoryCount} pct={promotedPct} unit="memories" />
                <LegendRow color="bg-purple-500" label="Private" count={memoryCount} pct={privatePct} unit="memories" />
              </div>
            </>
          )}

          {totalTokens === 0 && (
            <p className="text-xs text-muted-foreground">No context loaded yet.</p>
          )}
        </div>

        {/* M-04 Memory Breakdown */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <div>
            <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground mb-1">
              Memory Breakdown
            </p>
            <p className="text-2xl font-semibold tabular-nums text-foreground">
              {memoryCount === 0 && collectiveMemoryCount === 0
                ? "—"
                : String(memoryCount + collectiveMemoryCount)}
              {(memoryCount > 0 || collectiveMemoryCount > 0) && (
                <span className="text-sm font-normal text-muted-foreground ml-1">total</span>
              )}
            </p>
          </div>

          {(memoryCount > 0 || collectiveMemoryCount > 0 || pendingCount > 0) && (
            <>
              <MemoryPillBar
                promoted={collectiveMemoryCount}
                private_={privateMemCount}
                pending={pendingCount}
              />

              <div className="space-y-1">
                <LegendRow color="bg-teal-500" label="Promoted" count={collectiveMemoryCount} />
                <LegendRow color="bg-purple-500" label="Private" count={privateMemCount} />
                {pendingCount > 0 && <LegendRow color="bg-amber-400" label="Pending" count={pendingCount} />}
              </div>

              {pendingCount > 0 && (
                <div className="flex items-center gap-1.5 rounded-md bg-amber-500/10 border border-amber-500/20 px-2.5 py-1.5">
                  <div className="h-1.5 w-1.5 rounded-full bg-amber-400 shrink-0" />
                  <p className="text-xs text-amber-700 dark:text-amber-300">
                    {pendingCount} {pendingCount === 1 ? "memory" : "memories"} pending review
                  </p>
                </div>
              )}
            </>
          )}

          {memoryCount === 0 && collectiveMemoryCount === 0 && pendingCount === 0 && (
            <p className="text-xs text-muted-foreground">No memories yet.</p>
          )}
        </div>

        {/* M-08 Conversations */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            Conversations
          </p>

          {metricsLoading ? (
            <div className="space-y-2">
              <div className="h-8 w-16 rounded bg-muted animate-pulse" />
              <div className="h-3 w-24 rounded bg-muted animate-pulse" />
            </div>
          ) : (
            <>
              <div className="flex gap-4">
                <div>
                  <p className="text-2xl font-semibold tabular-nums text-foreground">
                    {metrics ? String(metrics.conversation_count) : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Conversations</p>
                </div>
                <div className="w-px bg-border" />
                <div>
                  <p className="text-2xl font-semibold tabular-nums text-foreground">
                    {metrics ? String(metrics.memory_extraction_count) : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Memories extracted</p>
                </div>
              </div>

              {metrics && metrics.conversation_count > 0 && (
                <p className="text-xs text-muted-foreground">
                  {metrics.extraction_rate.toFixed(1)} memories / conversation
                </p>
              )}

              {!metrics && (
                <p className="text-xs text-muted-foreground/60">No data yet</p>
              )}
            </>
          )}
        </div>
      </div>

      {/* Retrieval & Efficiency */}
      <div className="flex items-center gap-3 mb-4 mt-8">
        <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          Retrieval &amp; Efficiency
        </span>
        <div className="flex-1 h-px bg-border" />
        
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {/* M-02 Token Savings */}
        <div id="metric-token-savings" className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            Token Savings
          </p>

          {metricsLoading ? (
            <div className="space-y-2">
              <div className="h-8 w-16 rounded bg-muted animate-pulse" />
              <div className="h-3 w-24 rounded bg-muted animate-pulse" />
            </div>
          ) : (
            <>
              <div>
                <p className="text-2xl font-semibold tabular-nums text-green-600 dark:text-green-400">
                  {metrics ? `${(metrics.savings_pct * 100).toFixed(0)}%` : "—"}
                </p>
                <p className="text-xs text-muted-foreground mt-0.5">
                  {metrics ? `${metrics.total_requests} queries` : "No queries yet"}
                </p>
              </div>

              <div className="flex gap-3">
                <div className="rounded-md bg-muted px-2.5 py-1">
                  <p className="text-[10px] text-muted-foreground">P50</p>
                  <p className="text-xs font-semibold tabular-nums">—</p>
                </div>
                <div className="rounded-md bg-muted px-2.5 py-1">
                  <p className="text-[10px] text-muted-foreground">P99</p>
                  <p className="text-xs font-semibold tabular-nums">—</p>
                </div>
              </div>
            </>
          )}
        </div>

        {/* M-03 Context Match Quality */}
        <div id="metric-match-quality" className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            Context Match Quality
          </p>

          {metricsLoading ? (
            <div className="space-y-2">
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
            </div>
          ) : scoreTotal === 0 ? (
            <p className="text-xs text-muted-foreground">No retrieval data yet.</p>
          ) : (
            <div className="space-y-2">
              <QualityBar
                label="High"
                count={metrics?.score_high_total ?? 0}
                pct={highPct}
                color="bg-green-500"
              />
              <QualityBar
                label="Mid"
                count={metrics?.score_mid_total ?? 0}
                pct={midPct}
                color="bg-amber-400"
              />
              <QualityBar
                label="Low"
                count={metrics?.score_low_total ?? 0}
                pct={lowPct}
                color="bg-red-500"
              />
            </div>
          )}
        </div>

        {/* M-07 Quality vs Window Use */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            Quality vs Window Use
          </p>

          {metricsLoading ? (
            <div className="flex gap-4 justify-center">
              <div className="h-16 w-16 rounded-full bg-muted animate-pulse" />
              <div className="h-16 w-16 rounded-full bg-muted animate-pulse" />
            </div>
          ) : (
            <>
              <div className="flex items-end gap-4 justify-center">
                <ArcGauge
                  value={scoreTotal > 0 ? qualityScore : null}
                  label="Quality"
                  color="#22c55e"
                />
                <ArcGauge value={null} label="Window util." color="#94a3b8" />
              </div>

              {scoreTotal > 0 && (
                <div
                  className={cn(
                    "flex items-center gap-1.5 rounded-md border px-2.5 py-1.5",
                    qualityBanner.color,
                  )}
                >
                  <div className={cn("h-1.5 w-1.5 rounded-full shrink-0", qualityBanner.dot)} />
                  <p className="text-xs">{qualityBanner.label}</p>
                </div>
              )}
            </>
          )}
        </div>
      </div>

      {/* Usage */}
      <div className="flex items-center gap-3 mb-4 mt-8">
        <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          Usage
        </span>
        <div className="flex-1 h-px bg-border" />
        
      </div>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
        {/* M-05 Agents & Users */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            Agents &amp; Users
          </p>

          {metricsLoading ? (
            <div className="flex gap-6">
              <div className="space-y-1">
                <div className="h-8 w-12 rounded bg-muted animate-pulse" />
                <div className="h-3 w-16 rounded bg-muted animate-pulse" />
              </div>
              <div className="space-y-1">
                <div className="h-8 w-12 rounded bg-muted animate-pulse" />
                <div className="h-3 w-16 rounded bg-muted animate-pulse" />
              </div>
            </div>
          ) : (
            <>
              <div className="flex gap-6">
                <div>
                  <p className="text-2xl font-semibold tabular-nums text-foreground">
                    {metrics ? String(metrics.agent_count) : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Agents</p>
                </div>
                <div className="w-px bg-border" />
                <div>
                  <p className="text-2xl font-semibold tabular-nums text-foreground">
                    {metrics ? String(metrics.human_count) : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Human users</p>
                </div>
              </div>

              <p className="text-xs text-muted-foreground">
                Last accessed:{" "}
                <span className="text-foreground">
                  {formatRelativeTime(metrics?.last_accessed_at, nowMs)}
                </span>
              </p>
            </>
          )}
        </div>

        {/* M-06 Query Activity */}
        <div id="metric-query-activity">
        <ActivityCard
          activity={activity}
          totalQueryCount={totalQueryCount}
          isLoading={metricsLoading}
        />
        </div>
      </div>
    </div>
  );
}

type ActivityPoint = { date: string; hybrid: number; kb: number; memory: number };

const SERIES = [
  { key: "hybrid" as const, label: "Hybrid", color: "#6366f1" },
  { key: "kb"     as const, label: "Knowledge", color: "#14b8a6" },
  { key: "memory" as const, label: "Memory", color: "#a855f7" },
];

function ActivityCard({
  activity,
  totalQueryCount,
  isLoading,
}: {
  activity: ActivityPoint[];
  totalQueryCount: number;
  isLoading: boolean;
}) {
  const [chartType, setChartType] = useState<"line" | "bar">("line");

  return (
    <div className="rounded-xl border border-border bg-card p-4 space-y-3">
      <div className="flex items-center justify-between">
        <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
          Query Activity
        </p>
        <div className="flex rounded-md border border-border overflow-hidden">
          <button
            onClick={() => setChartType("line")}
            className={cn(
              "px-2 py-0.5 text-[10px] font-medium transition-colors",
              chartType === "line"
                ? "bg-primary text-primary-foreground"
                : "bg-background text-muted-foreground hover:text-foreground",
            )}
          >
            Line
          </button>
          <button
            onClick={() => setChartType("bar")}
            className={cn(
              "px-2 py-0.5 text-[10px] font-medium transition-colors border-l border-border",
              chartType === "bar"
                ? "bg-primary text-primary-foreground"
                : "bg-background text-muted-foreground hover:text-foreground",
            )}
          >
            Bar
          </button>
        </div>
      </div>

      {isLoading ? (
        <div className="space-y-2">
          <div className="h-8 w-16 rounded bg-muted animate-pulse" />
          <div className="h-16 w-full rounded bg-muted animate-pulse" />
        </div>
      ) : (
        <>
          <div>
            <p className="text-2xl font-semibold tabular-nums text-foreground">
              {totalQueryCount}
            </p>
            <p className="text-xs text-muted-foreground mt-0.5">Queries, last 30 days</p>
          </div>

          {activity.length > 0 ? (
            <>
              {chartType === "line" ? (
                <ActivityLineChart activity={activity} />
              ) : (
                <ActivityBarChart activity={activity} />
              )}
              <div className="flex items-center gap-3 flex-wrap">
                {SERIES.map((s) => (
                  <div key={s.key} className="flex items-center gap-1.5">
                    <div className="h-2 w-2 rounded-full shrink-0" style={{ background: s.color }} />
                    <span className="text-[10px] text-muted-foreground">{s.label}</span>
                  </div>
                ))}
              </div>
            </>
          ) : (
            <p className="text-xs text-muted-foreground">No activity recorded yet.</p>
          )}
        </>
      )}
    </div>
  );
}

function ActivityLineChart({ activity }: { activity: ActivityPoint[] }) {
  const W = 360;
  const H = 80;
  const LPAD = 26; // room for y-axis labels
  const PAD = 4;
  const DAYS = 30;

  const rawMax = Math.max(
    1,
    ...activity.flatMap((d) => SERIES.map((s) => d[s.key] as number)),
  );
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const yTicks = [0, Math.round(yMax / 2), yMax];

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD * 2;
  const xAt = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;

  const isSingle = activity.length === 1;

  return (
    <div>
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: 80 }}>
        {/* Y-axis gridlines + labels */}
        {yTicks.map((v) => {
          const gy = yAt(v);
          return (
            <g key={v}>
              <line x1={LPAD} y1={gy} x2={W - PAD} y2={gy}
                stroke="currentColor" strokeOpacity="0.1" strokeWidth="1" />
              <text x={LPAD - 4} y={gy + 3.5} textAnchor="end"
                fontSize="8" fill="currentColor" fillOpacity="0.45">{v}</text>
            </g>
          );
        })}

        {SERIES.map((s) => {
          if (isSingle) {
            const d = activity[0];
            const v = d[s.key] as number;
            if (v === 0) return null;
            return (
              <circle key={s.key} cx={xAt(dayOffset(d.date))} cy={yAt(v)}
                r="4" fill={s.color} />
            );
          }
          const pts = activity
            .map((d) => `${xAt(dayOffset(d.date)).toFixed(1)},${yAt(d[s.key] as number).toFixed(1)}`)
            .join(" ");
          return (
            <g key={s.key}>
              <polyline points={pts} fill="none" stroke={s.color}
                strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
              {activity.map((d) => {
                const v = d[s.key] as number;
                if (v === 0) return null;
                return (
                  <circle key={d.date} cx={xAt(dayOffset(d.date))} cy={yAt(v)}
                    r="3" fill={s.color} />
                );
              })}
            </g>
          );
        })}
      </svg>
      <div className="flex justify-between" style={{ paddingLeft: LPAD }}>
        <span className="text-[10px] text-muted-foreground">30d ago</span>
        <span className="text-[10px] text-muted-foreground">Today</span>
      </div>
    </div>
  );
}

function ActivityBarChart({ activity }: { activity: ActivityPoint[] }) {
  const [hovered, setHovered] = useState<ActivityPoint | null>(null);
  const [tipPct, setTipPct] = useState(0);

  const W = 360;
  const H = 80;
  const LPAD = 26;
  const PAD = 4;
  const DAYS = 30;
  const BAR_W = Math.max(6, Math.floor((W - LPAD - PAD) / DAYS) - 1);

  const rawMax = Math.max(1, ...activity.map((d) => d.hybrid + d.kb + d.memory));
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const yTicks = [0, Math.round(yMax / 2), yMax];

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD * 2;
  const barCx = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;
  const chartBottom = H - PAD;

  return (
    <div className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: 80 }}>
        {/* Y-axis gridlines + labels */}
        {yTicks.map((v) => {
          const gy = yAt(v);
          return (
            <g key={v}>
              <line x1={LPAD} y1={gy} x2={W - PAD} y2={gy}
                stroke="currentColor" strokeOpacity="0.1" strokeWidth="1" />
              <text x={LPAD - 4} y={gy + 3.5} textAnchor="end"
                fontSize="8" fill="currentColor" fillOpacity="0.45">{v}</text>
            </g>
          );
        })}

        {activity.map((d) => {
          const total = d.hybrid + d.kb + d.memory;
          if (total === 0) return null;
          const ago = dayOffset(d.date);
          const cx = barCx(ago);
          const x = cx - BAR_W / 2;
          const fullH = (total / yMax) * plotH;
          const segs = [
            { value: d.hybrid, color: SERIES[0].color },
            { value: d.kb,     color: SERIES[1].color },
            { value: d.memory, color: SERIES[2].color },
          ].filter((s) => s.value > 0);

          let yBottom = chartBottom;
          return (
            <g
              key={d.date}
              onMouseEnter={() => { setHovered(d); setTipPct(cx / W); }}
              onMouseLeave={() => setHovered(null)}
              style={{ cursor: "default" }}
            >
              {/* invisible wider hit area */}
              <rect x={x - 3} y={PAD} width={BAR_W + 6} height={plotH} fill="transparent" />
              {segs.map((seg, i) => {
                const segH = (seg.value / total) * fullH;
                const ry = yBottom - segH;
                yBottom -= segH;
                return (
                  <rect key={i} x={x} y={ry} width={BAR_W} height={segH}
                    fill={seg.color} rx={i === segs.length - 1 ? 1.5 : 0} />
                );
              })}
            </g>
          );
        })}
      </svg>

      {/* Hover tooltip */}
      {hovered && (
        <div
          className="absolute bottom-7 z-10 pointer-events-none rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{
            left: `${Math.min(80, Math.max(10, tipPct * 100))}%`,
            transform: "translateX(-50%)",
          }}
        >
          <p className="font-medium text-foreground mb-0.5">{hovered.date}</p>
          {hovered.hybrid > 0 && (
            <p className="text-muted-foreground">Hybrid <span className="text-foreground tabular-nums">{hovered.hybrid}</span></p>
          )}
          {hovered.kb > 0 && (
            <p className="text-muted-foreground">Knowledge <span className="text-foreground tabular-nums">{hovered.kb}</span></p>
          )}
          {hovered.memory > 0 && (
            <p className="text-muted-foreground">Memory <span className="text-foreground tabular-nums">{hovered.memory}</span></p>
          )}
        </div>
      )}

      <div className="flex justify-between mt-0.5" style={{ paddingLeft: LPAD }}>
        <span className="text-[10px] text-muted-foreground">30d ago</span>
        <span className="text-[10px] text-muted-foreground">Today</span>
      </div>
    </div>
  );
}

function LegendRow({
  color,
  label,
  count,
  pct,
  unit,
}: {
  color: string;
  label: string;
  count: number;
  pct?: number;
  unit?: string;
}) {
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      <div className={cn("h-2 w-2 rounded-full shrink-0", color)} />
      <span>{label}</span>
      <span className="ml-auto tabular-nums">
        {count}
        {unit ? ` ${unit}` : ""}
        {pct != null ? ` · ${Math.round(pct)}%` : ""}
      </span>
    </div>
  );
}

function MemoryPillBar({
  promoted,
  private_,
  pending,
}: {
  promoted: number;
  private_: number;
  pending: number;
}) {
  const total = promoted + private_ + pending;
  if (total === 0) return null;

  const promotedPct = (promoted / total) * 100;
  const privatePct = (private_ / total) * 100;
  const pendingPct = (pending / total) * 100;

  return (
    <div className="flex h-2 w-full overflow-hidden rounded-full bg-muted">
      <div className="bg-teal-500" style={{ width: `${promotedPct}%` }} />
      <div className="bg-purple-500" style={{ width: `${privatePct}%` }} />
      <div className="bg-amber-400" style={{ width: `${pendingPct}%` }} />
    </div>
  );
}

function QualityBar({
  label,
  count,
  pct,
  color,
}: {
  label: string;
  count: number;
  pct: number;
  color: string;
}) {
  return (
    <div className="space-y-1">
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>{label}</span>
        <span className="tabular-nums">
          {count} · {Math.round(pct)}%
        </span>
      </div>
      <div className="h-1.5 w-full rounded-full bg-muted overflow-hidden">
        <div className={cn("h-full rounded-full", color)} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}

function ArcGauge({
  value,
  label,
  color,
}: {
  value: number | null;
  label: string;
  color: string;
}) {
  const r = 28;
  const cx = 36;
  const cy = 36;
  const circumference = Math.PI * r;
  const pct = value !== null ? Math.min(1, Math.max(0, value)) : 0;
  const dashOffset = circumference * (1 - pct);

  return (
    <div className="flex flex-col items-center gap-1">
      <svg width="72" height="40" viewBox="0 0 72 40">
        <path
          d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
          fill="none"
          stroke="currentColor"
          strokeWidth="6"
          className="text-muted"
        />
        {value !== null && (
          <path
            d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
            fill="none"
            stroke={color}
            strokeWidth="6"
            strokeDasharray={circumference}
            strokeDashoffset={dashOffset}
            strokeLinecap="round"
          />
        )}
        <text
          x={cx}
          y={cy - 4}
          textAnchor="middle"
          fontSize="10"
          fontWeight="600"
          fill="currentColor"
          className="text-foreground"
        >
          {value !== null ? `${Math.round(value * 100)}%` : "—"}
        </text>
      </svg>
      <p className="text-[10px] text-muted-foreground">{label}</p>
    </div>
  );
}
