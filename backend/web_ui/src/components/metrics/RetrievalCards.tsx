// The retrieval cards: tokens saved per query, results by match quality, and how much of a model's
// context window a typical answer takes.

import { count } from "../../lib/format";
import { BAND_HIGH, BAND_MID, CONTEXT_WINDOW, formatTokens, pct, type Derived } from "../../lib/metrics";
import { BigStat, MetricCard } from "./MetricCard";

const NO_DATA: [string, string] = ["No retrieval data yet", "This fills in once agents search this datapack."];

export function EfficiencyCard({ d, requests }: { d: Derived; requests: number }) {
  const title = "Context efficiency";
  const subtitle = "Tokens sent vs. a fixed baseline per query";
  if (d.savedPct === null || d.avgTokensSent === null || d.avgTokensIfRaw === null) {
    return <MetricCard title={title} subtitle={subtitle} empty={NO_DATA} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <BigStat value={`${Math.round(d.savedPct)}%`} label={`saved across ${count(requests)} queries`} />
      <div className="mt-4 space-y-2 text-sm">
        <Bar label="Sent" value={d.avgTokensSent} max={d.avgTokensIfRaw} className="bg-primary" />
        <Bar label="Baseline" value={d.avgTokensIfRaw} max={d.avgTokensIfRaw} className="bg-muted-foreground/50" />
      </div>
      <p className="mt-3 text-xs text-muted-foreground">Average tokens per query.</p>
    </MetricCard>
  );
}

function Bar({ label, value, max, className }: { label: string; value: number; max: number; className: string }) {
  return (
    <div>
      <div className="flex justify-between text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular-nums">{formatTokens(value)}</span>
      </div>
      <div className="mt-1 h-2 rounded-full bg-muted">
        <div className={`h-2 rounded-full ${className}`} style={{ width: `${max > 0 ? (100 * value) / max : 0}%` }} />
      </div>
    </div>
  );
}

const BANDS = [
  { key: "high", label: `High (≥ ${BAND_HIGH})`, color: "bg-success" },
  { key: "mid", label: `Mid (≥ ${BAND_MID})`, color: "bg-warning" },
  { key: "low", label: "Low", color: "bg-destructive" },
] as const;

export function MatchQualityCard({ d }: { d: Derived }) {
  const s = d.scores;
  const title = "Context match quality";
  const subtitle = `Score bands across ${count(s.total)} results`;
  if (s.total === 0) return <MetricCard title={title} subtitle={subtitle} empty={NO_DATA} />;
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="flex h-2.5 overflow-hidden rounded-full bg-muted">
        {BANDS.map((b) => (s[b.key] > 0 ? <div key={b.key} className={b.color} style={{ width: `${(100 * s[b.key]) / s.total}%` }} /> : null))}
      </div>
      <div className="mt-4 space-y-1.5 text-sm">
        {BANDS.map((b) => (
          <div key={b.key} className="flex items-center gap-2">
            <span className={`size-2 rounded-full ${b.color}`} />
            <span className="text-muted-foreground">{b.label}</span>
            <span className="ml-auto tabular-nums">
              <b>{pct(s[b.key], s.total)}%</b> <span className="text-muted-foreground">· {count(s[b.key])}</span>
            </span>
          </div>
        ))}
      </div>
      <p className="mt-3 text-xs text-muted-foreground">By cosine similarity to the query.</p>
    </MetricCard>
  );
}

export function QualityWindowCard({ d }: { d: Derived }) {
  const title = "Quality vs window use";
  const subtitle = "Match score and context window utilization";
  if (d.qualityScore === null || d.windowUsed === null || d.avgTokensSent === null) {
    return <MetricCard title={title} subtitle={subtitle} empty={["No queries yet", "Run queries against this datapack to see match quality."]} />;
  }
  return (
    <MetricCard title={title} subtitle={subtitle}>
      <div className="flex gap-6">
        <BigStat value={`${Math.round(100 * d.qualityScore)}%`} label="High or mid matches" />
        <div className="w-px bg-border" />
        <BigStat value={`${(100 * d.windowUsed).toFixed(1)}%`} label={`of a ${formatTokens(CONTEXT_WINDOW)} window`} />
      </div>
      <p className="mt-4 text-xs text-muted-foreground">
        A typical answer sends {formatTokens(d.avgTokensSent)} tokens.
      </p>
    </MetricCard>
  );
}
