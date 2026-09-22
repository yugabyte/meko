// "Agents & Users" — who is reading this datapack, and when it was last
// touched. The Usage section's only card.

import { formatRelativeTime } from "@/lib/relativeTime";

import { MetricCard, MetricStat, MetricStatDivider, MetricStatRow } from "./MetricCard";

export function AgentsUsersCard({
  loading,
  agentCount,
  humanCount,
  lastAccessedAt,
  nowMs,
  className,
}: {
  loading: boolean;
  agentCount: number | null;
  humanCount: number | null;
  lastAccessedAt: string | null | undefined;
  nowMs: number;
  className?: string;
}) {
  return (
    <MetricCard
      title="Agents & Users"
      subtitle="Who is accessing this datapack"
      className={className}
    >
      {loading ? (
        <div className="flex gap-6">
          <div className="space-y-1">
            <div className="h-8 w-12 animate-pulse rounded bg-muted" />
            <div className="h-3 w-16 animate-pulse rounded bg-muted" />
          </div>
        </div>
      ) : (
        <>
          <MetricStatRow>
            <MetricStat value={agentCount != null ? String(agentCount) : "—"} label="Agents" />
            <MetricStatDivider />
            <MetricStat value={humanCount != null ? String(humanCount) : "—"} label="Human users" />
          </MetricStatRow>
          <p className="text-xs text-muted-foreground">
            Last accessed:{" "}
            <span className="text-foreground">{formatRelativeTime(lastAccessedAt, nowMs)}</span>
          </p>
        </>
      )}
    </MetricCard>
  );
}
