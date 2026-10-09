// Pure helpers for the compute resize flow: the default reservation shown as a
// placeholder, and the mapping from an AMP resize outcome to toast copy.

import type { AmpResizeOutcome } from "@/lib/api/ampApi";

// Default sizing the compute forms prefill: a small 0.5 vCPU / 4Gi cluster (a
// valid 8 GiB/vCPU ratio) that fits even the Free tier's 0.5-core cap, so the
// default create/add never overshoots. Also the resize placeholder when the
// controller hasn't reported an endpoint's current reservation.
export const DEFAULT_CPU = "0.5";
export const DEFAULT_MEMORY = "4Gi";

// AMP schedules endpoints as Guaranteed-QoS pods and requires at least this much
// memory per vCPU; a create/resize under the ratio is rejected 400 by the
// controller ("… below the 8 GiB per vCPU this control plane requires"). The
// create forms default + track memory to it so the golden path never trips it.
export const MEMORY_GIB_PER_VCPU = 8;

// coresFromCpu parses a Kubernetes CPU quantity ("2", "0.5", "500m") into a
// number of cores; NaN when it can't be parsed.
export function coresFromCpu(cpu: string): number {
  const s = cpu.trim();
  if (!s) return NaN;
  if (s.endsWith("m")) {
    const n = Number(s.slice(0, -1));
    return Number.isFinite(n) ? n / 1000 : NaN;
  }
  const n = Number(s);
  return Number.isFinite(n) ? n : NaN;
}

// minMemoryForCpu is AMP's minimum memory for a CPU size, as a whole-GiB
// Kubernetes quantity ("8Gi" for 1 vCPU, "4Gi" for 0.5). "" when the CPU can't
// be parsed, so callers can fall back to DEFAULT_MEMORY.
export function minMemoryForCpu(cpu: string): string {
  const cores = coresFromCpu(cpu);
  if (!Number.isFinite(cores) || cores <= 0) return "";
  return `${Math.ceil(cores * MEMORY_GIB_PER_VCPU)}Gi`;
}

// overBudgetMessage is the inline warning both compute forms show when the
// requested CPU exceeds the user's remaining vCPU. Kept here so the wizard and
// the add-compute form don't duplicate the copy (meko_ui/AGENTS.md).
export function overBudgetMessage(remaining: number | null): string {
  return `Exceeds your plan's remaining ${remaining} vCPU across your datapacks. Lower the vCPU or upgrade in Settings.`;
}

// defaultCpuForBudget is the CPU a compute form should prefill: the DEFAULT_CPU
// (0.5) sizing, capped at the user's remaining vCPU so the default is always
// within budget. An unlimited tier or unknown budget (null) takes DEFAULT_CPU.
// remaining <= 0 (at the cap) also returns DEFAULT_CPU — this function does NOT
// enforce the cap; the caller's overBudget check does (the wizard also greys its
// trigger, the add-compute form disables Provision), so an at-cap user who
// reaches here still can't submit.
export function defaultCpuForBudget(remaining: number | null): string {
  if (remaining == null || remaining <= 0) return DEFAULT_CPU;
  const capped = Math.min(coresFromCpu(DEFAULT_CPU), remaining);
  return String(Math.round(capped * 1000) / 1000);
}

// Outcome → toast copy, mirroring AMP's per-outcome messaging. Only "infeasible"
// is surfaced as an error (destructive); the rest are informational.
export function resizeToast(
  outcome: AmpResizeOutcome,
  size: string,
): { title: string; variant?: "destructive" } {
  switch (outcome) {
    case "completed":
      return { title: `Resized to ${size}` };
    case "pending":
      return { title: `Resize to ${size} scheduled (applies on the next restart)` };
    case "unsupported":
      return { title: `Resized to ${size} via a restart (live resize unsupported)` };
    case "infeasible":
      return {
        title: `Resize to ${size} isn't feasible on the current node`,
        variant: "destructive",
      };
  }
}
