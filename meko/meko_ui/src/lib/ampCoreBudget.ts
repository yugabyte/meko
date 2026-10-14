// Format the AMP vCPU core budget (account-wide: summed across all of the user's datapacks) as a tier-limit banner metric
// line ("X / Y vCPU used"). Shared by the AMP surfaces (create wizard, labs
// stage, add-compute) that render TierLimitBanner on a core-cap 402.
import type { AmpCoreBudget } from "@/lib/api/ampApi";

// max_cores/used_cores are float64(milli)/1000, so round to milli before display.
function vcpu(n: number): string {
  return String(Math.round(n * 1000) / 1000);
}

/** "X / Y vCPU used", or undefined when the tier is uncapped (max_cores null)
 *  or the budget hasn't loaded — the banner then shows no metric line. */
export function formatVcpuMetricLine(budget: AmpCoreBudget | undefined): string | undefined {
  if (!budget || budget.max_cores == null) return undefined;
  return `${vcpu(budget.used_cores)} / ${vcpu(budget.max_cores)} vCPU used`;
}
