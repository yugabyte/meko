/** Big-number "here's the point" card: real cost savings for this specific
 *  run, computed from real token counts (PrReviewDemoApp) against published
 *  Bedrock rates (bedrockPricing.ts). Only rendered once both costs are
 *  known and positive -- see PrReviewDemoApp's guard before mounting this. */
export function CostPayoffCard({
  savingsPct,
  slmCost,
  slmModelLabel,
  claudeCost,
  claudeModelLabel,
}: {
  savingsPct: number;
  slmCost: number;
  slmModelLabel: string;
  claudeCost: number;
  claudeModelLabel: string;
}) {
  return (
    <div className="flex flex-col gap-4 rounded-md border border-emerald-500/30 bg-emerald-500/5 p-4">
      <span className="text-[11px] font-semibold uppercase tracking-wide text-emerald-700 dark:text-emerald-300">
        Estimated cost for this review
      </span>
      <div className="flex flex-wrap items-center gap-6">
        <div>
          <div className="font-mono text-4xl font-semibold leading-none text-emerald-600 dark:text-emerald-400">
            {savingsPct}%
          </div>
          <div className="mt-1 text-xs text-muted-foreground">less spend</div>
        </div>
        <div className="flex min-w-[200px] flex-1 flex-col gap-1.5 border-l border-emerald-500/25 pl-5">
          <div className="flex items-center justify-between text-sm">
            <span className="text-muted-foreground">{claudeModelLabel}</span>
            <span className="font-mono text-muted-foreground line-through decoration-muted-foreground/60">
              ${claudeCost.toFixed(4)}
            </span>
          </div>
          <div className="flex items-center justify-between text-sm">
            <span className="text-muted-foreground">{slmModelLabel}</span>
            <span className="font-mono text-base font-semibold text-emerald-600 dark:text-emerald-400">
              ${slmCost.toFixed(4)}
            </span>
          </div>
        </div>
      </div>
      <p className="text-[11px] text-muted-foreground">
        From this run's real token counts and published Bedrock on-demand rates -- see
        bedrockPricing.ts.
      </p>
    </div>
  );
}
