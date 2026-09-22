// "Context Efficiency" — what one query actually sends versus what an
// unfiltered search would have. First of the three Retrieval & Efficiency
// cards; shares MetricCard's header/body/footer bands with the other two so
// the row lines up.

import type { ReactNode } from "react";

import { formatTokens } from "@/lib/tokenSavings";
import {
  Tooltip,
  TooltipContent,
  TooltipProvider,
  TooltipTrigger,
} from "@/components/ui/tooltip";
import { MiniSparkline } from "@/components/ui/charts/MiniSparkline";
import type { ActivityPoint } from "@/components/ui/charts/chartSeries";

import { MetricCard } from "./MetricCard";

export function ContextEfficiencyCard({
  loading,
  savedPct,
  avgTokensSent,
  avgTokensIfRaw,
  avgTokensSaved,
  totalRequests,
  activity,
}: {
  loading: boolean;
  /** 0–1. Null when there is no retrieval data to derive it from. */
  savedPct: number | null;
  avgTokensSent: number | null;
  avgTokensIfRaw: number | null;
  avgTokensSaved: number | null;
  totalRequests: number;
  activity: ActivityPoint[];
}) {
  const hasSavings = savedPct !== null && avgTokensSent !== null && avgTokensIfRaw !== null;

  return (
    <MetricCard
      id="metric-token-savings"
      title="Context Efficiency"
      subtitle="Tokens sent vs. fixed baseline per query"
      empty={
        !loading && !hasSavings
          ? {
              title: "No retrieval data yet",
              description: "Savings appear once agents start querying this datapack.",
            }
          : undefined
      }
    >
      {loading ? (
        <div className="space-y-3">
          <div className="h-5 w-full animate-pulse rounded bg-muted" />
          <div className="h-10 w-full animate-pulse rounded bg-muted" />
          <div className="h-8 w-full animate-pulse rounded bg-muted" />
        </div>
      ) : !hasSavings ? null : (
        <>
          {/* Stacked compression bar */}
          <div className="space-y-1.5">
            <div className="flex h-5 w-full gap-px overflow-hidden rounded-full">
              <div
                className="bg-muted-foreground/20 transition-all"
                style={{ width: `${(1 - savedPct) * 100}%` }}
                title={`Sent: ~${formatTokens(avgTokensSent)} tokens/query`}
              />
              <div
                className="bg-success transition-all"
                style={{ width: `${savedPct * 100}%` }}
                title={
                  avgTokensSaved !== null
                    ? `Saved: ~${formatTokens(avgTokensSaved)} tokens/query`
                    : undefined
                }
              />
            </div>
            <div className="flex justify-between text-[10px] text-muted-foreground">
              <span className="flex items-center gap-1">
                <span className="inline-block h-2 w-2 rounded-sm bg-muted-foreground/30" />
                Sent to Claude
              </span>
              <span className="font-semibold tabular-nums text-success">
                {Math.round(savedPct * 100)}% filtered out
              </span>
              <span className="flex items-center gap-1">
                Saved
                <span className="inline-block h-2 w-2 rounded-sm bg-success" />
              </span>
            </div>
          </div>

          {/* Per-query stats */}
          <div className="flex gap-3">
            <PerQueryStat
              label="Avg sent / query"
              value={`~${formatTokens(avgTokensSent)}`}
              caption="tokens"
              tooltip="Avg tokens actually passed to Claude per query — the compressed context after Meko retrieves, re-ranks, and filters the matched chunks. This is what your LLM sees."
            />
            <PerQueryStat
              label="Baseline / query"
              value={`~${formatTokens(avgTokensIfRaw)}`}
              caption="matched chunks × 1K tokens / query"
              tooltip="What a raw (unfiltered) KB search would have cost: the chunks Meko actually matched for that query × 1,000 tokens each. For memory-only queries with no KB search, a flat 10K token estimate is used as the fallback."
            />
          </div>

          {/* Query activity sparkline */}
          <div className="mt-auto">
            <p className="mb-1 text-[10px] text-muted-foreground">
              Query trend · {totalRequests.toLocaleString()} total
            </p>
            <MiniSparkline activity={activity} />
          </div>
        </>
      )}
    </MetricCard>
  );
}

/** Both boxes are the same shape, and `min-h` on the label keeps their values
 *  on one line whether the label wraps or not. */
function PerQueryStat({
  label,
  value,
  caption,
  tooltip,
}: {
  label: string;
  value: ReactNode;
  caption: string;
  tooltip: string;
}) {
  return (
    <div className="flex-1 rounded-lg bg-muted/60 px-3 py-2 text-center">
      <p className="mb-0.5 flex min-h-[1.5rem] items-center justify-center gap-0.5 text-[10px] text-muted-foreground">
        {label}
        <TooltipProvider delayDuration={100}>
          <Tooltip>
            <TooltipTrigger asChild>
              <span className="cursor-help opacity-50 hover:opacity-100">ⓘ</span>
            </TooltipTrigger>
            <TooltipContent side="top" className="max-w-[240px] text-center leading-snug">
              {tooltip}
            </TooltipContent>
          </Tooltip>
        </TooltipProvider>
      </p>
      <p className="text-sm font-bold tabular-nums">{value}</p>
      <p className="min-h-[2rem] text-[10px] leading-tight text-muted-foreground">{caption}</p>
    </div>
  );
}
