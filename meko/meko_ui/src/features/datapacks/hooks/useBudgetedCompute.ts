// Shared CPU/memory state for the two AMP compute forms (the create-cluster
// wizard and the per-branch add-compute form): prefill from the user's remaining
// tier vCPU until they edit it, keep memory at AMP's 8 GiB/vCPU floor, and report
// whether the current sizing is within budget. Extracted so the guard logic
// (and its edges) live in ONE place rather than being copied into both forms.

import { useEffect, useMemo, useState } from "react";
import { useAmpCoreBudget } from "./useAmpClusters";
import {
  DEFAULT_CPU,
  DEFAULT_MEMORY,
  coresFromCpu,
  defaultCpuForBudget,
  minMemoryForCpu,
} from "../components/datapack-detail/datapack-clusters/resize";

export type BudgetedCompute = {
  cpu: string;
  memory: string;
  /** Set CPU (marks it user-edited; re-derives memory unless memory was edited). */
  setCpu: (value: string) => void;
  /** Set memory (marks it user-edited so the prefill/CPU-change stops touching it). */
  setMemory: (value: string) => void;
  /** Remaining vCPU across the user's datapacks; null = unlimited tier / unknown. */
  remaining: number | null;
  /** cpu parses to more cores than remaining. */
  overBudget: boolean;
  /** cpu+memory are non-empty, cpu is a positive number, and within budget. */
  valid: boolean;
  /** The budget query is still loading — callers should hold submit until it resolves. */
  isLoading: boolean;
  /** Reset edited flags + fields to the defaults (e.g. when a dialog reopens). */
  reset: () => void;
};

export function useBudgetedCompute(
  datapackId: string,
  options?: { enabled?: boolean },
): BudgetedCompute {
  const budgetQuery = useAmpCoreBudget(datapackId, { enabled: options?.enabled ?? true });

  const remaining = useMemo(() => {
    const b = budgetQuery.data;
    if (!b || b.max_cores == null) return null; // unlimited tier or not yet known
    // Round to milli precision: max_cores/used_cores are float64(milli)/1000 on
    // the wire, so 0.5 − 0.2 would otherwise be 0.30000000000000004 and leak that
    // 17-digit literal into the CPU field and the copy.
    return Math.max(0, Math.round((b.max_cores - b.used_cores) * 1000) / 1000);
  }, [budgetQuery.data]);

  const [cpu, setCpuState] = useState(DEFAULT_CPU);
  const [memory, setMemoryState] = useState(DEFAULT_MEMORY);
  const [cpuEdited, setCpuEdited] = useState(false);
  const [memoryEdited, setMemoryEdited] = useState(false);

  // Prefill CPU + its ratio-matched memory from the remaining budget until the
  // user edits them, so gated users default within budget and at a valid ratio.
  useEffect(() => {
    if (cpuEdited) return;
    const cpuDefault = defaultCpuForBudget(remaining);
    setCpuState(cpuDefault);
    if (!memoryEdited) setMemoryState(minMemoryForCpu(cpuDefault) || DEFAULT_MEMORY);
  }, [remaining, cpuEdited, memoryEdited]);

  const setCpu = (value: string) => {
    setCpuEdited(true);
    setCpuState(value);
    // Keep memory at AMP's floor as CPU changes — but never clobber a memory the
    // user has typed themselves.
    if (!memoryEdited) {
      const mem = minMemoryForCpu(value);
      if (mem) setMemoryState(mem);
    }
  };

  const setMemory = (value: string) => {
    setMemoryEdited(true);
    setMemoryState(value);
  };

  const reset = () => {
    setCpuEdited(false);
    setMemoryEdited(false);
    // Seed straight from the current budget rather than DEFAULT_*: the prefill
    // effect keys off remaining/cpuEdited, so it won't re-fire on a reopen where
    // neither changed — reset must therefore set the derived defaults itself. If
    // the budget resolves later (a gated tier), the effect re-seeds from it.
    const cpuDefault = defaultCpuForBudget(remaining);
    setCpuState(cpuDefault);
    setMemoryState(minMemoryForCpu(cpuDefault) || DEFAULT_MEMORY);
  };

  const cores = coresFromCpu(cpu);
  const overBudget = remaining != null && Number.isFinite(cores) && cores > remaining + 1e-9;
  // A blank/unparseable CPU is NOT valid: the server counts "" as AMP's 2-core
  // default, so an empty field would slip past overBudget (cores is NaN) and 402
  // a gated user — the exact failure this guard exists to prevent.
  const valid =
    Boolean(cpu.trim() && memory.trim()) && Number.isFinite(cores) && cores > 0 && !overBudget;

  return {
    cpu,
    memory,
    setCpu,
    setMemory,
    remaining,
    overBudget,
    valid,
    isLoading: budgetQuery.isLoading,
    reset,
  };
}
