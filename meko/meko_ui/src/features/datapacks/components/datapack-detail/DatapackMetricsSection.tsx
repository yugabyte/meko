import { useState } from "react";
import { useNavigate, type NavigateFunction } from "react-router-dom";
import { cn } from "@/lib/utils";
import { ArrowRight, Database, RefreshCw, Sparkles, Zap } from "lucide-react";
import { Card } from "@/components/ui/card";
import { useDatapackMetrics } from "@/features/datapacks/hooks/useDatapackMetrics";
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from "@/components/ui/tooltip";
import { EfficiencyTile } from "@/components/efficiency-tile";
import { formatRelativeTime, useNowMs } from "@/lib/relativeTime";
import { datapackDetailUrlOpenConnect, datapackKnowledgePath, datapackLearningsPath, ROUTES } from "@/app/routes/paths";
import { CONTEXTUAL_PROMINENCE_SEARCH_ENABLED } from "@/lib/tokenSavings";

type Props = {
  datapackId: string | null;
  knowledgeCount: number;
  memoryCount: number;
  collectiveMemoryCount: number;
  learningsCount: number;
};

// Matches KB_CHUNK_COUNT_ESTIMATE in meko-mcp-server/src/tools.py.
// Used only when real chunk counts are unavailable — estimates chunks per KB file.
const KB_CHUNKS_PER_FILE = 10;
// Matches KB_CHUNK_TOKEN_ESTIMATE — tokens per chunk.
const KB_TOKENS_PER_CHUNK = 1024;

function formatTokens(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${Math.round(n / 1_000)}K`;
  return String(n);
}

function formatKB(kb: number): string {
  if (kb >= 1_000_000) return `${(kb / 1_000_000).toFixed(1)} GB`;
  if (kb >= 1_000) return `${(kb / 1_000).toFixed(1)} MB`;
  return `${Math.round(kb)} KB`;
}

// ---------------------------------------------------------------------------
// DatapackOnboardingStepper — full-width "get started" banner shown when a
// datapack has no content, no searches, and no quality data yet.
// ---------------------------------------------------------------------------
const ACCENT_CLS = {
  violet:  { num: "bg-violet-500/15 text-violet-400",   arrow: "text-violet-400"  },
  amber:   { num: "bg-amber-500/15 text-amber-500",     arrow: "text-amber-500"   },
  emerald: { num: "bg-emerald-500/15 text-emerald-400", arrow: "text-emerald-400" },
};

function OnboardingStepCard({
  num,
  title,
  description,
  accent,
  path,
  navigate,
}: {
  num: number;
  title: string;
  description: string;
  accent: keyof typeof ACCENT_CLS;
  path: string | null;
  navigate: NavigateFunction;
}) {
  const cls = ACCENT_CLS[accent];
  return (
    <button
      type="button"
      disabled={!path}
      onClick={() => path && navigate(path)}
      className={cn(
        "group flex flex-col gap-3 rounded-lg border border-border p-4 text-left w-full transition-colors",
        path ? "cursor-pointer hover:border-foreground/25 hover:bg-muted/40" : "cursor-default opacity-50",
      )}
    >
      <div className="flex items-center justify-between">
        <span className={cn("flex h-6 w-6 items-center justify-center rounded-md text-xs font-semibold", cls.num)}>
          {num}
        </span>
        {path ? (
          <ArrowRight className={cn("h-3.5 w-3.5 opacity-0 transition-opacity group-hover:opacity-100", cls.arrow)} />
        ) : null}
      </div>
      <div>
        <p className="text-sm font-medium text-foreground">{title}</p>
        <p className="mt-0.5 text-xs text-muted-foreground">{description}</p>
      </div>
    </button>
  );
}

function DatapackOnboardingStepper({
  datapackId,
  navigate,
}: {
  datapackId: string | null;
  navigate: NavigateFunction;
}) {
  const connectPath = datapackId ? datapackDetailUrlOpenConnect(datapackId) : ROUTES.connect;

  const chip = (
    label: string,
    accent: keyof typeof ACCENT_CLS,
    path: string | null,
  ) => (
    <button
      type="button"
      disabled={!path}
      onClick={() => path && navigate(path)}
      className={cn(
        "inline-flex items-center gap-1.5 rounded-full border px-3 py-1 text-xs font-medium transition-colors",
        path ? "cursor-pointer hover:bg-muted/60" : "cursor-default opacity-40",
        accent === "violet"  && "border-violet-500/30 text-violet-400 hover:border-violet-500/50",
        accent === "amber"   && "border-amber-500/30 text-amber-500 hover:border-amber-500/50",
        accent === "emerald" && "border-emerald-500/30 text-emerald-400 hover:border-emerald-500/50",
      )}
    >
      {label}
      {path ? <ArrowRight className="h-3 w-3" /> : null}
    </button>
  );

  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 rounded-lg border border-border bg-card px-4 py-3">
      <span className="text-xs text-muted-foreground shrink-0">Get started:</span>
      {chip("Add knowledge documents", "violet", datapackId ? datapackKnowledgePath(datapackId) : null)}
      <span className="text-[10px] text-muted-foreground/50 shrink-0">·</span>
      {chip("Connect with your agents", "amber", connectPath)}
      <span className="text-[10px] font-medium text-muted-foreground/60 shrink-0">or</span>
      {chip("Try Meko Labs", "emerald", ROUTES.labs)}
    </div>
  );
}

// ---------------------------------------------------------------------------
// DatapackMetricsTilesRow — 3 EfficiencyTile cards for the datapack overview
// ---------------------------------------------------------------------------
export function DatapackMetricsTilesRow({
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
  const navigate = useNavigate();
  const { data: metrics, isLoading } = useDatapackMetrics(datapackId);

  // Total context
  const kbTokens = (metrics?.kb_chunk_count ?? 0) > 0
    ? metrics!.kb_chunk_count * KB_TOKENS_PER_CHUNK
    : knowledgeCount * KB_CHUNKS_PER_FILE * KB_TOKENS_PER_CHUNK;
  const memTokens = metrics
    ? metrics.memory_chunk_count * KB_TOKENS_PER_CHUNK
    : (memoryCount + collectiveMemoryCount) * 200;
  const convTokens = metrics ? metrics.conversation_chunk_count * KB_TOKENS_PER_CHUNK : 0;
  const totalTokens = kbTokens + memTokens + convTokens;

  // Context efficiency
  const hasSavings = metrics && metrics.total_requests > 0 && (metrics.tokens_if_raw_total ?? 0) > 0;
  const savingsPct = hasSavings ? Math.round(metrics!.savings_pct * 100) : null;
  const avgSent = hasSavings
    ? Math.round((metrics!.tokens_if_raw_total - metrics!.tokens_saved_total) / metrics!.total_requests)
    : null;
  const avgBaseline = hasSavings
    ? Math.round(metrics!.tokens_if_raw_total / metrics!.total_requests)
    : null;

  // Context quality
  const scoreTotal =
    (metrics?.score_high_total ?? 0) +
    (metrics?.score_mid_total ?? 0) +
    (metrics?.score_low_total ?? 0);
  const highMidPct = scoreTotal > 0
    ? Math.round(((metrics!.score_high_total + metrics!.score_mid_total) / scoreTotal) * 100)
    : null;

  const isEmpty = !isLoading && totalTokens === 0 && scoreTotal === 0 && !hasSavings;

  if (isEmpty) {
    return <DatapackOnboardingStepper datapackId={datapackId} navigate={navigate} />;
  }

  return (
    <div className="grid grid-cols-1 sm:grid-cols-3 gap-4">
      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.("metric-context-size")}
      >
        <EfficiencyTile
          icon={<Database className="h-4 w-4" />}
          title="Total Context"
          accent="violet"
          loading={isLoading}
          isEmpty={!isLoading && totalTokens === 0}
          emptyLabel="No content indexed yet."
          headline={totalTokens > 0 ? formatTokens(totalTokens) + " tokens" : "—"}
          subtitle="KB · memory · conversations"
          bars={[]}
        />
      </div>

      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.("section-retrieval-efficiency")}
      >
        <EfficiencyTile
          icon={<Zap className="h-4 w-4" />}
          title="Context Efficiency"
          badge={hasSavings ? "live" : undefined}
          accent="emerald"
          loading={isLoading}
          isEmpty={!isLoading && !hasSavings}
          emptyLabel="No contextual searches recorded yet."
          headline={savingsPct != null ? `${savingsPct}% saved` : "—"}
          subtitle={avgSent != null && avgBaseline != null
            ? `~${formatTokens(avgSent)} sent vs ~${formatTokens(avgBaseline)} baseline / query`
            : "vs fixed baseline per query"}
          bars={[]}
        />
      </div>

      <div
        className="cursor-pointer"
        onClick={() => onNavigateToMetric?.("section-retrieval-efficiency")}
      >
        <EfficiencyTile
          icon={<Sparkles className="h-4 w-4" />}
          title="Context Quality"
          accent="amber"
          loading={isLoading}
          isEmpty={!isLoading && scoreTotal === 0}
          emptyLabel="No retrieval data yet."
          headline={highMidPct != null ? `${highMidPct}% relevant` : "—"}
          subtitle={scoreTotal > 0 ? `across ${scoreTotal.toLocaleString()} results` : "High + Mid matches"}
          bars={[]}
        />
      </div>
    </div>
  );
}

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

  const kbTokens = knowledgeCount * KB_CHUNKS_PER_FILE * KB_TOKENS_PER_CHUNK;
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

  const hasSavings = metrics && metrics.total_requests > 0 && (metrics.tokens_if_raw_total ?? 0) > 0;
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
  const navigate = useNavigate();

  // Token estimates: use real chunk counts from API (1024 tokens/chunk) when available,
  // fall back to prop-based estimates until first load.
  const kbTokens = (metrics?.kb_chunk_count ?? 0) > 0
    ? metrics!.kb_chunk_count * KB_TOKENS_PER_CHUNK
    : knowledgeCount * KB_CHUNKS_PER_FILE * KB_TOKENS_PER_CHUNK;
  const memTokens = metrics
    ? metrics.memory_chunk_count * KB_TOKENS_PER_CHUNK
    : (memoryCount + collectiveMemoryCount) * 200;
  const convTokens = metrics ? metrics.conversation_chunk_count * KB_TOKENS_PER_CHUNK : 0;
  const totalTokens = kbTokens + memTokens + convTokens;

  const kbPct = totalTokens > 0 ? (kbTokens / totalTokens) * 100 : 0;
  const memPct = totalTokens > 0 ? (memTokens / totalTokens) * 100 : 0;
  const convPct = totalTokens > 0 ? (convTokens / totalTokens) * 100 : 0;

  // Storage sizes (bytes → KB)
  const kbSizeKB = metrics ? metrics.kb_bytes / 1024 : 0;
  const memSizeKB = metrics ? metrics.memory_bytes / 1024 : 0;
  const convSizeKB = metrics ? metrics.conversation_bytes / 1024 : 0;
  const totalSizeKB = kbSizeKB + memSizeKB + convSizeKB;

  const kbSizePct = totalSizeKB > 0 ? (kbSizeKB / totalSizeKB) * 100 : 0;
  const memSizePct = totalSizeKB > 0 ? (memSizeKB / totalSizeKB) * 100 : 0;
  const convSizePct = totalSizeKB > 0 ? (convSizeKB / totalSizeKB) * 100 : 0;

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

  // M-07 quality score — high+mid combined for the gauge; high-only for the banner thresholds
  const qualityScore = scoreTotal > 0
    ? ((metrics?.score_high_total ?? 0) + (metrics?.score_mid_total ?? 0)) / scoreTotal
    : 0;
  // Banner thresholds on High+Mid: ≥80% = strong, ≥50% = moderate, <50% = needs attention
  const qualityBanner =
    qualityScore > 0.8
      ? { label: "Strong retrieval", color: "bg-green-500/10 border-green-500/20 text-green-700 dark:text-green-300", dot: "bg-green-500" }
      : qualityScore > 0.5
        ? { label: "Moderate", color: "bg-amber-500/10 border-amber-500/20 text-amber-700 dark:text-amber-300", dot: "bg-amber-400" }
        : { label: "Needs attention", color: "bg-red-500/10 border-red-500/20 text-red-700 dark:text-red-300", dot: "bg-red-500" };

  // M-06 daily activity
  const activity = metrics?.daily_activity ?? [];
  const totalQueryCount = activity.reduce((s, d) => s + d.hybrid + d.kb + d.memory, 0);

  // M-02 token savings derived values
  const hasSavings = metrics && metrics.total_requests > 0 && (metrics.tokens_if_raw_total ?? 0) > 0;
  const savedPct = hasSavings ? metrics!.savings_pct : 0;
  const sentPct = 1 - savedPct;
  const avgTokensIfRaw = hasSavings
    ? Math.round(metrics!.tokens_if_raw_total / metrics!.total_requests)
    : null;
  const avgTokensSent = hasSavings
    ? Math.round((metrics!.tokens_if_raw_total - metrics!.tokens_saved_total) / metrics!.total_requests)
    : null;
  function fmtTokens(n: number): string {
    if (n >= 1000) return `${(n / 1000).toFixed(n >= 10000 ? 0 : 1)}K`;
    return n.toLocaleString();
  }

  const CONTEXT_WINDOW = 128_000;
  const windowUsed = avgTokensSent != null ? avgTokensSent / CONTEXT_WINDOW : null;

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
          className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          title="Refresh metrics"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
          Refresh
        </button>
      </div>

      {/* Row 1 — 3 cards */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {/* 1. Total Context by Tokens */}
        <div id="metric-context-size" className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Total Context by Tokens</p>
            <p className="text-xs text-muted-foreground mt-0.5">Estimated token footprint of all stored content</p>
          </div>

          <p className="text-3xl font-bold tabular-nums text-foreground">
            {totalTokens === 0 ? "—" : formatTokens(totalTokens)}
            {totalTokens > 0 && (
              <span className="text-base font-normal text-muted-foreground ml-1">tokens</span>
            )}
          </p>

          {totalTokens > 0 ? (
            <>
              <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
                <div className="bg-blue-500" style={{ width: `${kbPct}%` }} />
                <div className="bg-purple-400" style={{ width: `${memPct}%` }} />
                <div className="bg-teal-400" style={{ width: `${convPct}%` }} />
              </div>
              <div className="space-y-1.5">
                <ContextLegendRow color="bg-blue-500" label="Knowledge Base" value={formatTokens(kbTokens)} pct={kbPct} />
                <ContextLegendRow color="bg-purple-400" label="Memory" value={formatTokens(memTokens)} pct={memPct} />
                <ContextLegendRow color="bg-teal-400" label="Conversations" value={formatTokens(convTokens)} pct={convPct} />
              </div>
            </>
          ) : (
            <p className="text-xs text-muted-foreground">No context loaded yet.</p>
          )}
        </div>

        {/* 2. Total Context by Size */}
        <div className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Total Context by Size</p>
            <p className="text-xs text-muted-foreground mt-0.5">Actual storage footprint of indexed content</p>
          </div>

          <p className="text-3xl font-bold tabular-nums text-foreground">
            {totalSizeKB === 0 ? "—" : formatKB(totalSizeKB)}
          </p>

          {totalSizeKB > 0 ? (
            <>
              <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
                <div className="bg-blue-500" style={{ width: `${kbSizePct}%` }} />
                <div className="bg-purple-400" style={{ width: `${memSizePct}%` }} />
                <div className="bg-teal-400" style={{ width: `${convSizePct}%` }} />
              </div>
              <div className="space-y-1.5">
                <ContextLegendRow color="bg-blue-500" label="Knowledge Base" value={formatKB(kbSizeKB)} pct={kbSizePct} />
                <ContextLegendRow color="bg-purple-400" label="Memory" value={formatKB(memSizeKB)} pct={memSizePct} />
                <ContextLegendRow color="bg-teal-400" label="Conversations" value={formatKB(convSizeKB)} pct={convSizePct} />
              </div>
            </>
          ) : (
            <p className="text-xs text-muted-foreground">No data available yet.</p>
          )}
        </div>

        {/* 3. Memory Breakdown */}
        <div className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Memory Breakdown</p>
            <p className="text-xs text-muted-foreground mt-0.5">Shared vs private vs pending promotion</p>
          </div>

          {(memoryCount > 0 || collectiveMemoryCount > 0 || pendingCount > 0) ? (
            <>
              <MemoryPillBar
                promoted={collectiveMemoryCount}
                private_={privateMemCount}
                pending={pendingCount}
              />

              <div className="flex flex-wrap gap-x-4 gap-y-1">
                <div className="flex items-center gap-1.5">
                  <div className="h-2.5 w-2.5 rounded-full bg-teal-400 shrink-0" />
                  <span className="text-xs text-muted-foreground">Promoted</span>
                  <span className="text-xs font-semibold tabular-nums text-foreground">{collectiveMemoryCount}</span>
                </div>
                <div className="flex items-center gap-1.5">
                  <div className="h-2.5 w-2.5 rounded-full bg-purple-500 shrink-0" />
                  <span className="text-xs text-muted-foreground">Private</span>
                  <span className="text-xs font-semibold tabular-nums text-foreground">{privateMemCount}</span>
                </div>
                {pendingCount > 0 && (
                  <div className="flex items-center gap-1.5">
                    <div className="h-2.5 w-2.5 rounded-full bg-amber-400 shrink-0" />
                    <span className="text-xs text-muted-foreground">Pending</span>
                    <span className="text-xs font-semibold tabular-nums text-foreground">{pendingCount}</span>
                  </div>
                )}
              </div>

              <div className="flex gap-6">
                <div>
                  <p className="text-2xl font-bold tabular-nums text-foreground">
                    {(memoryCount + collectiveMemoryCount).toLocaleString()}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Total memories</p>
                </div>
                {(memoryCount + collectiveMemoryCount) > 0 && (
                  <>
                    <div className="w-px bg-border" />
                    <div>
                      <p className="text-2xl font-bold tabular-nums text-foreground">
                        {Math.round((collectiveMemoryCount / (memoryCount + collectiveMemoryCount)) * 100)}%
                      </p>
                      <p className="text-xs text-muted-foreground mt-0.5">Shared</p>
                    </div>
                  </>
                )}
              </div>

              {pendingCount > 0 && (
                <button
                  onClick={() => datapackId && navigate(datapackLearningsPath(datapackId))}
                  className="flex items-start gap-2 w-full text-left rounded-md bg-amber-500/10 border border-amber-500/20 px-3 py-2 hover:bg-amber-500/20 transition-colors"
                >
                  <span className="text-amber-500 text-xs shrink-0 mt-px">⚠</span>
                  <p className="text-xs text-amber-700 dark:text-amber-300">
                    {pendingCount} {pendingCount === 1 ? "memory" : "memories"} pending review — promote to share with all agents
                  </p>
                </button>
              )}
            </>
          ) : (
            <p className="text-xs text-muted-foreground">No memories yet.</p>
          )}
        </div>
      </div>

      {/* Row 2 — 2 cards */}
      <div className="grid grid-cols-1 md:grid-cols-2 gap-4 mt-4">
        {/* 4. Knowledge Base */}
        <div className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Knowledge Base</p>
            <p className="text-xs text-muted-foreground mt-0.5">Indexed chunks, files and storage</p>
          </div>

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
                  <p className="text-3xl font-bold tabular-nums text-foreground">
                    {metrics ? metrics.kb_chunk_count.toLocaleString() : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Vector chunks</p>
                </div>
                <div className="w-px bg-border" />
                <div>
                  <p className="text-3xl font-bold tabular-nums text-foreground">
                    {metrics ? metrics.kb_file_count.toLocaleString() : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Files</p>
                </div>
              </div>
              <p className="text-xs text-muted-foreground">
                Total size:{" "}
                <span className="text-foreground font-medium">
                  {metrics && metrics.kb_bytes > 0 ? formatKB(metrics.kb_bytes / 1024) : "—"}
                </span>
              </p>
            </>
          )}
        </div>

        {/* 5. Captured & Extracted */}
        <div className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Captured &amp; Extracted</p>
            <p className="text-xs text-muted-foreground mt-0.5">Conversations indexed into this datapack</p>
          </div>

          {metricsLoading ? (
            <div className="space-y-2">
              <div className="h-8 w-32 rounded bg-muted animate-pulse" />
              <div className="h-3 w-24 rounded bg-muted animate-pulse" />
            </div>
          ) : (
            <>
              <div className="flex gap-6">
                <div>
                  <p className="text-3xl font-bold tabular-nums text-foreground">
                    {metrics ? metrics.conversation_count.toLocaleString() : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Conversations</p>
                </div>
                <div className="w-px bg-border" />
                <div>
                  <p className="text-3xl font-bold tabular-nums text-foreground">
                    {metrics ? metrics.memory_extraction_count.toLocaleString() : "—"}
                  </p>
                  <p className="text-xs text-muted-foreground mt-0.5">Memories extracted</p>
                </div>
              </div>

              {metrics && metrics.conversation_count > 0 && (
                <div className="space-y-1.5">
                  <p className="text-xs font-medium text-muted-foreground">Extraction rate</p>
                  <div className="h-1 w-full rounded-full bg-muted overflow-hidden">
                    <div
                      className="h-full rounded-full bg-purple-500"
                      style={{ width: `${Math.min(100, (metrics.extraction_rate / 5) * 100)}%` }}
                    />
                  </div>
                  <p className="text-xs text-muted-foreground">
                    {metrics.extraction_rate.toFixed(2)} memories / conversation
                  </p>
                </div>
              )}

            </>
          )}
        </div>
      </div>

      {/* Retrieval & Efficiency */}
      <div id="section-retrieval-efficiency" className="flex items-center gap-3 mb-4 mt-8">
        <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          Retrieval &amp; Efficiency
        </span>
        <div className="flex-1 h-px bg-border" />
        <button
          onClick={() => refetch()}
          disabled={isFetching}
          className="flex items-center gap-1 text-xs text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
          title="Refresh metrics"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", isFetching && "animate-spin")} />
          Refresh
        </button>
      </div>

      {/* Context Queries — first in section */}
      <div className="mb-4" id="metric-query-activity">
        <ActivityCard
          activity={activity}
          totalQueryCount={totalQueryCount}
          isLoading={metricsLoading}
          title="Context Queries"
        />
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4 items-stretch">
        {/* M-02 Input Token Savings */}
        <div id="metric-token-savings" className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Context Efficiency</p>
            <p className="text-xs text-muted-foreground mt-0.5">Tokens sent vs. fixed baseline per query</p>
          </div>

          {metricsLoading ? (
            <div className="space-y-3">
              <div className="h-5 w-full rounded bg-muted animate-pulse" />
              <div className="h-10 w-full rounded bg-muted animate-pulse" />
              <div className="h-8 w-full rounded bg-muted animate-pulse" />
            </div>
          ) : !hasSavings ? (
            <p className="text-xs text-muted-foreground">No retrieval data yet.</p>
          ) : (
            <>
              {/* Stacked compression bar */}
              <div className="space-y-1.5">
                <div className="flex h-5 w-full rounded-full overflow-hidden gap-px">
                  <div
                    className="bg-muted-foreground/20 flex items-center justify-center transition-all"
                    style={{ width: `${sentPct * 100}%` }}
                    title={`Sent: ~${fmtTokens(avgTokensSent!)} tokens/query`}
                  />
                  <div
                    className="bg-emerald-400 flex items-center justify-center transition-all"
                    style={{ width: `${savedPct * 100}%` }}
                    title={`Saved: ~${fmtTokens(Math.round(metrics!.tokens_saved_total / metrics!.total_requests))} tokens/query`}
                  />
                </div>
                <div className="flex justify-between text-[10px] text-muted-foreground">
                  <span className="flex items-center gap-1">
                    <span className="inline-block h-2 w-2 rounded-sm bg-muted-foreground/30" />
                    Sent to Claude
                  </span>
                  <span className="font-semibold text-emerald-500 tabular-nums">
                    {Math.round(savedPct * 100)}% filtered out
                  </span>
                  <span className="flex items-center gap-1">
                    Saved
                    <span className="inline-block h-2 w-2 rounded-sm bg-emerald-400" />
                  </span>
                </div>
              </div>

              {/* Per-query stats */}
              <div className="flex gap-3">
                <div className="rounded-lg bg-muted/60 px-3 py-2 flex-1 text-center">
                  <p className="text-[10px] text-muted-foreground mb-0.5 flex items-center justify-center gap-0.5">
                    Avg sent / query
                    <TooltipProvider delayDuration={100}>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="opacity-50 hover:opacity-100 cursor-help">ⓘ</span>
                        </TooltipTrigger>
                        <TooltipContent side="top" className="max-w-[240px] text-center leading-snug">
                          Avg tokens actually passed to Claude per query — the compressed context
                          after Meko retrieves, re-ranks, and filters the matched chunks.
                          This is what your LLM sees.
                        </TooltipContent>
                      </Tooltip>
                    </TooltipProvider>
                  </p>
                  <p className="text-sm font-bold tabular-nums">~{fmtTokens(avgTokensSent!)}</p>
                  <p className="text-[10px] text-muted-foreground">tokens</p>
                </div>
                <div className="rounded-lg bg-muted/60 px-3 py-2 flex-1 text-center">
                  <p className="text-[10px] text-muted-foreground mb-0.5 flex items-center justify-center gap-0.5">
                    Baseline / query
                    <TooltipProvider delayDuration={100}>
                      <Tooltip>
                        <TooltipTrigger asChild>
                          <span className="opacity-50 hover:opacity-100 cursor-help">ⓘ</span>
                        </TooltipTrigger>
                        <TooltipContent side="top" className="max-w-[240px] text-center leading-snug">
                          What a raw (unfiltered) KB search would have cost: the chunks Meko
                          actually matched for that query × 1,000 tokens each. For
                          memory-only queries with no KB search, a flat 10K token estimate
                          is used as the fallback.
                        </TooltipContent>
                      </Tooltip>
                    </TooltipProvider>
                  </p>
                  <p className="text-sm font-bold tabular-nums">~{fmtTokens(avgTokensIfRaw!)}</p>
                  <p className="text-[10px] text-muted-foreground">matched chunks × 1K tokens / query</p>
                </div>
              </div>

              {/* Query activity sparkline */}
              <div>
                <p className="text-[10px] text-muted-foreground mb-1">
                  Query trend · {metrics!.total_requests.toLocaleString()} total
                </p>
                <MiniSparkline activity={activity} />
              </div>
            </>
          )}
        </div>

        {/* M-03 Context Match Quality */}
        <div id="metric-match-quality" className="rounded-xl border border-border bg-card p-5 space-y-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Context Match Quality</p>
            <p className="text-xs text-muted-foreground mt-0.5">
              Score bands across {scoreTotal.toLocaleString()} results
            </p>
          </div>

          {metricsLoading ? (
            <div className="space-y-2">
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
              <div className="h-3 w-full rounded bg-muted animate-pulse" />
            </div>
          ) : scoreTotal === 0 ? (
            <p className="text-xs text-muted-foreground">No retrieval data yet.</p>
          ) : (
            <>
              <div className="flex gap-4 items-center">
                {/* Donut chart */}
                <DonutChart
                  high={metrics?.score_high_total ?? 0}
                  mid={metrics?.score_mid_total ?? 0}
                  low={metrics?.score_low_total ?? 0}
                  total={scoreTotal}
                />
                {/* Legend */}
                <div className="space-y-2 flex-1">
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-1.5">
                      <div className="h-1 w-5 rounded-full bg-emerald-400" />
                      <span className="text-xs text-muted-foreground">High</span>
                    </div>
                    <span className="text-xs font-semibold tabular-nums">{(metrics?.score_high_total ?? 0).toLocaleString()}</span>
                  </div>
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-1.5">
                      <div className="h-1 w-5 rounded-full bg-amber-400" />
                      <span className="text-xs text-muted-foreground">Mid</span>
                    </div>
                    <span className="text-xs font-semibold tabular-nums">{(metrics?.score_mid_total ?? 0).toLocaleString()}</span>
                  </div>
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-1.5">
                      <div className="h-1 w-5 rounded-full bg-red-400" />
                      <span className="text-xs text-muted-foreground">Low</span>
                    </div>
                    <span className="text-xs font-semibold tabular-nums">{(metrics?.score_low_total ?? 0).toLocaleString()}</span>
                  </div>
                </div>
              </div>

              {/* Summary banner */}
              {(highPct + midPct) >= 80 ? (
                <div className="flex items-center gap-2 rounded-lg bg-emerald-500/10 border border-emerald-500/20 px-3 py-2">
                  <span className="text-emerald-400 text-xs">✓</span>
                  <p className="text-xs text-emerald-700 dark:text-emerald-300">
                    {Math.round(highPct + midPct)}% of queries return High or Mid quality matches
                  </p>
                </div>
              ) : (
                <div className="flex items-center gap-2 rounded-lg bg-amber-500/10 border border-amber-500/20 px-3 py-2">
                  <span className="text-amber-400 text-xs">⚠</span>
                  <p className="text-xs text-amber-700 dark:text-amber-300">
                    {Math.round(lowPct)}% of queries return Low quality matches
                  </p>
                </div>
              )}
            </>
          )}
        </div>

        {/* M-07 Quality vs Window Use */}
        <div className="rounded-xl border border-border bg-card p-5 flex flex-col gap-4">
          <div>
            <p className="text-lg font-bold text-foreground leading-tight">Quality vs Window Use</p>
            <p className="text-xs text-muted-foreground mt-0.5">Match score and context window utilization</p>
          </div>

          {metricsLoading ? (
            <div className="flex gap-6 justify-center flex-1">
              <div className="h-20 w-20 rounded-full bg-muted animate-pulse" />
              <div className="h-20 w-20 rounded-full bg-muted animate-pulse" />
            </div>
          ) : (
            <>
              <div className="flex items-end gap-4 justify-center flex-1">
                <ArcGauge
                  value={scoreTotal > 0 ? qualityScore : null}
                  label="Match score"
                  color="#22c55e"
                  pill={scoreTotal > 0 ? "High + Mid" : undefined}
                  tooltip="% of queries that returned High or Mid quality matches. High = strong semantic match; Mid = relevant but less precise."
                />
                <ArcGauge
                  value={windowUsed}
                  label="Window used"
                  color="#60a5fa"
                  pill={windowUsed != null ? "of 128K" : undefined}
                  tooltip={`Avg tokens sent to Claude per query as a share of the 128K context window. Lower is better — Meko keeps this small so your LLM has room for the conversation.${avgTokensSent != null ? ` Current avg: ~${fmtTokens(avgTokensSent)} tokens/query.` : ""}`}
                />
              </div>

              <div className={cn(
                "flex items-start gap-2 rounded-lg border px-3 py-2 mt-auto",
                scoreTotal > 0 ? qualityBanner.color : "border-border bg-muted/30",
              )}>
                <span className="text-sm mt-px">
                  {scoreTotal === 0 ? "○" : qualityScore > 0.8 ? "✦" : qualityScore > 0.5 ? "◈" : "●"}
                </span>
                <div>
                  <p className="text-xs font-semibold">
                    {scoreTotal === 0 ? "No queries yet" : qualityBanner.label}
                  </p>
                  <p className="text-xs opacity-80 mt-0.5">
                    {scoreTotal === 0
                      ? "Run queries against this datapack to see match quality."
                      : qualityScore > 0.8
                        ? "High + Mid quality matches — retrieval is working well."
                        : qualityScore > 0.5
                          ? "Moderate matches. Consider refining your knowledge base content."
                          : "Low match quality. Review and improve indexed content."}
                  </p>
                </div>
              </div>
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

      {/* M-05 Agents & Users */}
      <div className="rounded-xl border border-border bg-card p-5 space-y-4 md:w-1/2">
        <div>
          <p className="text-lg font-bold text-foreground leading-tight">Agents &amp; Users</p>
          <p className="text-xs text-muted-foreground mt-0.5">Who is accessing this datapack</p>
        </div>

        {metricsLoading ? (
          <div className="flex gap-6">
            <div className="space-y-1">
              <div className="h-8 w-12 rounded bg-muted animate-pulse" />
              <div className="h-3 w-16 rounded bg-muted animate-pulse" />
            </div>
          </div>
        ) : (
          <>
            <div className="flex gap-6">
              <div>
                <p className="text-3xl font-bold tabular-nums text-foreground">
                  {metrics ? String(metrics.agent_count) : "—"}
                </p>
                <p className="text-xs text-muted-foreground mt-0.5">Agents</p>
              </div>
              <div className="w-px bg-border" />
              <div>
                <p className="text-3xl font-bold tabular-nums text-foreground">
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
  title = "Query Activity",
}: {
  activity: ActivityPoint[];
  totalQueryCount: number;
  isLoading: boolean;
  title?: string;
}) {
  const [chartType, setChartType] = useState<"line" | "bar">("bar");

  return (
    <div className="rounded-xl border border-border bg-card p-5 space-y-4">
      {/* Header row */}
      <div className="flex items-center justify-between">
        <div>
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            {title}
          </p>
          {!isLoading && (
            <p className="text-muted-foreground mt-0.5">
              <span className="text-foreground text-2xl font-bold tabular-nums">{totalQueryCount.toLocaleString()}</span>
              <span className="text-sm ml-1">queries</span>
            </p>
          )}
        </div>
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

      {/* Chart — full width */}
      {isLoading ? (
        <div className="h-32 w-full rounded bg-muted animate-pulse" />
      ) : activity.length > 0 ? (
        <>
          {chartType === "line" ? (
            <ActivityLineChart activity={activity} tall />
          ) : (
            <ActivityBarChart activity={activity} tall={true} />
          )}
          <div className="flex items-center gap-4 flex-wrap">
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
    </div>
  );
}

function ActivityLineChart({ activity, tall }: { activity: ActivityPoint[]; tall?: boolean }) {
  const W = 360;
  const BPAD = 14;
  const H = (tall ? 160 : 80) + BPAD;
  const PAD = 4;

  const rawMax = Math.max(
    1,
    ...activity.flatMap((d) => SERIES.map((s) => d[s.key] as number)),
  );
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const tickCount = yMax <= 10 ? 2 : yMax <= 50 ? 3 : 4;
  const yTicks = Array.from({ length: tickCount }, (_, i) => Math.round((yMax / (tickCount - 1)) * i));
  const axisFontSize = yMax <= 10 ? 11 : yMax <= 50 ? 9 : 8;
  const LPAD = yMax >= 1000 ? 36 : yMax >= 100 ? 30 : 26;

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  // Span from oldest data point; cap at 30 days; add 1-day padding on each side
  const maxDaysAgo = activity.length > 0 ? Math.max(...activity.map((d) => dayOffset(d.date))) : 30;
  const DAYS = Math.min(30, Math.max(maxDaysAgo + 1, 2));
  const oldestLabel = DAYS >= 30 ? "30d ago" : DAYS === 1 ? "Yesterday" : `${DAYS}d ago`;

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD - BPAD;
  const xAt = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;

  const isSingle = activity.length === 1;

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: H }}>
      {yTicks.map((v) => {
        const gy = yAt(v);
        return (
          <g key={v}>
            <line x1={LPAD} y1={gy} x2={W - PAD} y2={gy}
              stroke="currentColor" strokeOpacity="0.1" strokeWidth="1" />
            <text x={LPAD - 4} y={gy + axisFontSize * 0.35} textAnchor="end"
              fontSize={axisFontSize} fill="currentColor" fillOpacity="0.5">{v}</text>
          </g>
        );
      })}

      <text x={LPAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="start">{oldestLabel}</text>
      <text x={W - PAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="end">Today</text>

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
  );
}

function ActivityBarChart({ activity, tall }: { activity: ActivityPoint[]; tall?: boolean }) {
  const [hovered, setHovered] = useState<ActivityPoint | null>(null);
  const [tipPct, setTipPct] = useState(0);

  const W = 360;
  const BPAD = 14;
  const H = (tall ? 160 : 80) + BPAD;
  const PAD = 4;

  const rawMax = Math.max(1, ...activity.map((d) => d.hybrid + d.kb + d.memory));
  const yMax = rawMax <= 5 ? 5 : rawMax <= 10 ? 10 : Math.ceil(rawMax / 5) * 5;
  const tickCount = yMax <= 10 ? 2 : yMax <= 50 ? 3 : 4;
  const yTicks = Array.from({ length: tickCount }, (_, i) => Math.round((yMax / (tickCount - 1)) * i));
  const axisFontSize = yMax <= 10 ? 11 : yMax <= 50 ? 9 : 8;
  const LPAD = yMax >= 1000 ? 36 : yMax >= 100 ? 30 : 26;

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dayOffset = (dateStr: string) => {
    const d = new Date(dateStr);
    d.setHours(0, 0, 0, 0);
    return Math.round((today.getTime() - d.getTime()) / 86_400_000);
  };

  const maxDaysAgo = activity.length > 0 ? Math.max(...activity.map((d) => dayOffset(d.date))) : 30;
  const DAYS = Math.min(30, Math.max(maxDaysAgo + 1, 2));
  const oldestLabel = DAYS >= 30 ? "30d ago" : DAYS === 1 ? "Yesterday" : `${DAYS}d ago`;
  const BAR_W = Math.max(6, Math.floor((W - LPAD - PAD) / DAYS) - 1);

  const plotW = W - LPAD - PAD;
  const plotH = H - PAD - BPAD;
  const barCx = (daysAgo: number) => LPAD + ((DAYS - daysAgo) / DAYS) * plotW;
  const yAt = (v: number) => PAD + (1 - v / yMax) * plotH;
  const chartBottom = PAD + plotH;

  return (
    <div className="relative">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" style={{ height: H }}>
        {/* Y-axis gridlines + labels */}
        {yTicks.map((v) => {
          const gy = yAt(v);
          return (
            <g key={v}>
              <line x1={LPAD} y1={gy} x2={W - PAD} y2={gy}
                stroke="currentColor" strokeOpacity="0.1" strokeWidth="1" />
              <text x={LPAD - 4} y={gy + axisFontSize * 0.35} textAnchor="end"
                fontSize={axisFontSize} fill="currentColor" fillOpacity="0.5">{v}</text>
            </g>
          );
        })}

        <text x={LPAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="start">{oldestLabel}</text>
        <text x={W - PAD} y={H - 3} fontSize="9" fill="currentColor" fillOpacity="0.4" textAnchor="end">Today</text>

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
          className="absolute z-10 pointer-events-none rounded-md border border-border bg-popover px-2.5 py-1.5 text-xs shadow-md"
          style={{
            bottom: BPAD + 4,
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
    <div className="flex h-3 w-full overflow-hidden rounded-full bg-muted">
      <div className="bg-teal-400" style={{ width: `${promotedPct}%` }} />
      <div className="bg-purple-500" style={{ width: `${privatePct}%` }} />
      <div className="bg-amber-400" style={{ width: `${pendingPct}%` }} />
    </div>
  );
}

function ContextLegendRow({
  color,
  label,
  value,
  pct,
}: {
  color: string;
  label: string;
  value: string;
  pct: number;
}) {
  return (
    <div className="flex items-center gap-2 text-xs text-muted-foreground">
      <div className={cn("h-2 w-2 rounded-full shrink-0", color)} />
      <span>{label}</span>
      <span className="ml-auto tabular-nums text-foreground/80">
        <span className="font-semibold">{Math.round(pct)}%</span>
        <span className="text-muted-foreground ml-1">· {value}</span>
      </span>
    </div>
  );
}

type DailyActivity = { date: string; hybrid: number; kb: number; memory: number };

function MiniSparkline({ activity }: { activity: DailyActivity[] }) {
  if (activity.length === 0) {
    return <div className="h-8 w-full rounded bg-muted/40 flex items-center justify-center">
      <span className="text-[10px] text-muted-foreground/40">No data</span>
    </div>;
  }

  const totals = activity.map(d => d.hybrid + d.kb + d.memory);
  const max = Math.max(...totals, 1);
  const w = 100 / totals.length;

  const points = totals.map((v, i) => {
    const x = i * w + w / 2;
    const y = 32 - (v / max) * 28;
    return `${x},${y}`;
  });
  const polyline = points.join(" ");

  const areaPoints = [
    `0,32`,
    ...points,
    `100,32`,
  ].join(" ");

  return (
    <svg viewBox="0 0 100 32" className="w-full h-8" preserveAspectRatio="none">
      <polygon points={areaPoints} fill="rgb(168 85 247 / 0.15)" />
      <polyline points={polyline} fill="none" stroke="rgb(168 85 247)" strokeWidth="1.5" strokeLinejoin="round" strokeLinecap="round" />
    </svg>
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


function DonutChart({
  high,
  mid,
  low,
  total,
}: {
  high: number;
  mid: number;
  low: number;
  total: number;
}) {
  const r = 30;
  const cx = 40;
  const cy = 40;
  const circumference = 2 * Math.PI * r;

  // Colors via Tailwind oklch tokens (never raw hex per meko/REVIEW.md)
  const segments = [
    { value: high, color: "oklch(var(--color-emerald-400))" },
    { value: mid,  color: "oklch(var(--color-amber-400))"  },
    { value: low,  color: "oklch(var(--color-red-400))"    },
  ];

  let offset = 0;
  const arcs = segments.map((seg) => {
    const pct = total > 0 ? seg.value / total : 0;
    const dash = pct * circumference;
    const arc = { dash, offset, color: seg.color };
    offset += dash;
    return arc;
  });

  return (
    <div className="relative flex-shrink-0">
      <svg width="80" height="80" viewBox="0 0 80 80">
        <circle cx={cx} cy={cy} r={r} fill="none" stroke="currentColor" strokeWidth="10" className="text-muted" />
        {arcs.map((arc, i) => (
          <circle
            key={i}
            cx={cx}
            cy={cy}
            r={r}
            fill="none"
            stroke={arc.color}
            strokeWidth="10"
            strokeDasharray={`${arc.dash} ${circumference - arc.dash}`}
            strokeDashoffset={circumference / 4 - arc.offset}
            style={{ transform: "rotate(-90deg)", transformOrigin: `${cx}px ${cy}px` }}
          />
        ))}
        <text x={cx} y={cy + 4} textAnchor="middle" fontSize="12" fontWeight="700" fill="currentColor">
          {total.toLocaleString()}
        </text>
      </svg>
    </div>
  );
}

function ArcGauge({
  value,
  label,
  color,
  pill,
  tooltip,
}: {
  value: number | null;
  label: string;
  color: string;
  pill?: string;
  tooltip?: string;
}) {
  const r = 32;
  const cx = 42;
  const cy = 42;
  const circumference = Math.PI * r;
  const pct = value !== null ? Math.min(1, Math.max(0, value)) : 0;
  const dashOffset = circumference * (1 - pct);

  return (
    <div className="flex flex-col items-center gap-1">
      <svg width="84" height="48" viewBox="0 0 84 48">
        <path
          d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
          fill="none"
          stroke="currentColor"
          strokeWidth="8"
          className="text-muted"
        />
        {value !== null && (
          <path
            d={`M ${cx - r} ${cy} A ${r} ${r} 0 0 1 ${cx + r} ${cy}`}
            fill="none"
            stroke={color}
            strokeWidth="8"
            strokeDasharray={circumference}
            strokeDashoffset={dashOffset}
            strokeLinecap="round"
          />
        )}
        <text
          x={cx}
          y={cy - 6}
          textAnchor="middle"
          fontSize="11"
          fontWeight="700"
          fill="currentColor"
        >
          {value !== null ? `${Math.round(value * 100)}%` : "—"}
        </text>
      </svg>
      <p className="text-[10px] text-muted-foreground flex items-center gap-0.5">
        {label}
        {tooltip && (
          <TooltipProvider delayDuration={100}>
            <Tooltip>
              <TooltipTrigger asChild>
                <span className="opacity-50 hover:opacity-100 cursor-help">ⓘ</span>
              </TooltipTrigger>
              <TooltipContent side="top" className="max-w-[240px] text-center leading-snug">
                {tooltip}
              </TooltipContent>
            </Tooltip>
          </TooltipProvider>
        )}
      </p>
      {pill && (
        <span className="text-[10px] font-semibold px-1.5 py-0.5 rounded-full bg-muted text-muted-foreground">
          {pill}
        </span>
      )}
    </div>
  );
}
