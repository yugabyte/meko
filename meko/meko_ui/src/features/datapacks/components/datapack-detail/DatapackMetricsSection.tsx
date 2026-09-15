import { cn } from "@/lib/utils";

type Props = {
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

export function DatapackMetricsSection({
  knowledgeCount,
  memoryCount,
  collectiveMemoryCount,
  learningsCount,
}: Props) {
  const kbTokens = knowledgeCount * 1000;
  const privateMemTokens = memoryCount * 200;
  const promotedMemTokens = collectiveMemoryCount * 200;
  const totalTokens = kbTokens + privateMemTokens + promotedMemTokens;

  const kbPct = totalTokens > 0 ? (kbTokens / totalTokens) * 100 : 0;
  const promotedPct = totalTokens > 0 ? (promotedMemTokens / totalTokens) * 100 : 0;
  const privatePct = totalTokens > 0 ? (privateMemTokens / totalTokens) * 100 : 0;

  const privateMemCount = Math.max(0, memoryCount - collectiveMemoryCount);
  const pendingCount = learningsCount;

  return (
    <div>
      <div className="flex items-center gap-3 mb-4">
        <span className="text-xs font-semibold uppercase tracking-widest text-muted-foreground">
          Meko Context
        </span>
        <div className="flex-1 h-px bg-border" />
        <span className="text-xs text-muted-foreground">M-01 · M-04 · M-08</span>
      </div>

      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        {/* M-01 Total Context Size */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <div>
            <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground mb-1">
              M-01 · Total Context Size
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
                <div
                  className="bg-blue-500"
                  style={{ width: `${kbPct}%` }}
                />
                <div
                  className="bg-teal-500"
                  style={{ width: `${promotedPct}%` }}
                />
                <div
                  className="bg-purple-500"
                  style={{ width: `${privatePct}%` }}
                />
              </div>

              <div className="space-y-1">
                <LegendRow
                  color="bg-blue-500"
                  label="Knowledge"
                  count={knowledgeCount}
                  pct={kbPct}
                  unit="chunks"
                />
                <LegendRow
                  color="bg-teal-500"
                  label="Promoted"
                  count={collectiveMemoryCount}
                  pct={promotedPct}
                  unit="memories"
                />
                <LegendRow
                  color="bg-purple-500"
                  label="Private"
                  count={memoryCount}
                  pct={privatePct}
                  unit="memories"
                />
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
              M-04 · Memory Breakdown
            </p>
            <p className="text-2xl font-semibold tabular-nums text-foreground">
              {memoryCount === 0 && collectiveMemoryCount === 0 ? "—" : String(memoryCount + collectiveMemoryCount)}
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
                {pendingCount > 0 && (
                  <LegendRow color="bg-amber-400" label="Pending" count={pendingCount} />
                )}
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

        {/* M-08 Conversations (stub) */}
        <div className="rounded-xl border border-border bg-card p-4 space-y-3">
          <p className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground">
            M-08 · Conversations
          </p>

          <div className="flex gap-4">
            <div>
              <p className="text-2xl font-semibold text-muted-foreground/50">—</p>
              <p className="text-xs text-muted-foreground mt-0.5">Conversations</p>
            </div>
            <div className="w-px bg-border" />
            <div>
              <p className="text-2xl font-semibold text-muted-foreground/50">—</p>
              <p className="text-xs text-muted-foreground mt-0.5">Memories extracted</p>
            </div>
          </div>

          <p className="text-xs text-muted-foreground/60">Data coming soon</p>
        </div>
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
