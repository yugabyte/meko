// The reviews run during THIS lab session, with their real per-run numbers.
// Separate from the dashboard's all-time aggregates because nothing persists
// per-run tokens/cost/findings yet — these come from the runs LabStageSlot
// is still holding in memory, and are gone on reload.

import { estimateCost } from "../../lib/bedrockPricing";
import type { PrReviewRunState } from "../demo-apps/PrReviewDemoApp";
import { countableFindings } from "../../lib/countableFindings";
import { MODEL_LABELS } from "../demo-apps/ReviewAgentConfigPanel";

export function SessionRunsTable({ runs }: { runs: PrReviewRunState[] }) {
  const completed = runs.filter((run) => run.reviewUsage && run.selectedModelId);

  if (completed.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-border px-3 py-4 text-sm text-muted-foreground">
        No reviews run in this session yet. Each one you run appears here with its real token count.
      </p>
    );
  }

  return (
    <div className="overflow-x-auto">
      <table className="w-full min-w-[28rem] text-sm">
        <thead>
          <tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
            <th className="pb-2 font-medium">Model</th>
            <th className="pb-2 text-right font-medium">Input</th>
            <th className="pb-2 text-right font-medium">Output</th>
            <th className="pb-2 text-right font-medium">Cost</th>
            <th className="pb-2 text-right font-medium">Findings</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-border">
          {completed.map((run, index) => {
            const usage = run.reviewUsage!;
            const modelId = run.selectedModelId!;
            const cost = estimateCost(modelId, usage);
            const findings = countableFindings(run.reviewFindings)?.length ?? null;
            return (
              <tr key={run.conversationId ?? index}>
                <td className="py-2">{MODEL_LABELS[modelId]}</td>
                <td className="py-2 text-right font-mono tabular-nums">
                  {usage.input_tokens.toLocaleString()}
                </td>
                <td className="py-2 text-right font-mono tabular-nums">
                  {usage.output_tokens.toLocaleString()}
                </td>
                <td className="py-2 text-right font-mono tabular-nums">
                  {cost === null ? "—" : `$${cost.toFixed(4)}`}
                </td>
                <td className="py-2 text-right font-mono tabular-nums">{findings ?? "—"}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
