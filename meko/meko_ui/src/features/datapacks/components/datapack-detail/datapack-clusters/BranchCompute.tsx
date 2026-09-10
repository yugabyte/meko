// The Compute section under a branch: lists provisioned compute and the
// controls to provision another. Enforces AMP's one-read-write-per-branch rule.

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { ampCoreLimitMessage } from "@/lib/api/ampApi";
import { useCompute, useCreateCompute } from "@/features/datapacks/hooks/useAmpClusters";
import { useBudgetedCompute } from "@/features/datapacks/hooks/useBudgetedCompute";
import { ComputeInstance } from "./ComputeInstance";
import { CoreLimitNotice } from "./CoreLimitNotice";
import { overBudgetMessage } from "./resize";

export function BranchCompute({
  datapackId,
  clusterId,
  branchId,
  branchName,
  isDefaultBranch,
}: {
  datapackId: string;
  clusterId: string;
  branchId: string;
  branchName: string;
  isDefaultBranch: boolean;
}) {
  const computeQuery = useCompute(datapackId, clusterId, branchId);
  const instances = computeQuery.data ?? [];
  const loading = computeQuery.isLoading;
  const createMut = useCreateCompute(datapackId, clusterId, branchId);

  const [role, setRole] = useState<"read_write" | "read_only">("read_write");
  // CPU / memory sizing + tier-budget guard, shared with the create-cluster
  // wizard (prefill within budget, memory at AMP's ratio, blank-CPU guard).
  const budget = useBudgetedCompute(datapackId);
  const { cpu, memory } = budget;
  const [provisionError, setProvisionError] = useState<{ message: string; upgrade: boolean } | null>(
    null,
  );
  const loadError = computeQuery.error
    ? getApiErrorMessage(computeQuery.error, "Failed to load compute.")
    : null;

  // A branch admits at most one read-write compute (multiple read-only are
  // fine). Once one exists, disable the read-write option and force new
  // provisions to read-only — same rule AMP enforces.
  const hasReadWrite = instances.some((ep) => ep.role === "read_write");
  useEffect(() => {
    if (hasReadWrite && role === "read_write") setRole("read_only");
  }, [hasReadWrite, role]);

  const provision = async () => {
    if (createMut.isPending) return;
    setProvisionError(null);
    try {
      // Mutation invalidates the compute query, so the list refreshes itself.
      await createMut.mutateAsync({ role, resources: { cpu, memory } });
    } catch (e) {
      const upgradeMsg = ampCoreLimitMessage(e);
      setProvisionError(
        upgradeMsg
          ? { message: upgradeMsg, upgrade: true }
          : { message: getApiErrorMessage(e, "Failed to provision compute."), upgrade: false },
      );
    }
  };

  return (
    <div className="mt-2 border-t border-border pt-2">
      <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
        Compute
      </div>
      {loading ? (
        <p className="text-xs text-muted-foreground">Loading compute…</p>
      ) : instances.length === 0 ? (
        <p className="text-xs text-muted-foreground">No compute instances yet.</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {instances.map((ep) => (
            <ComputeInstance
              key={ep.id}
              ep={ep}
              datapackId={datapackId}
              clusterId={clusterId}
              branchId={branchId}
              branchName={branchName}
              isDefaultBranch={isDefaultBranch}
            />
          ))}
        </ul>
      )}
      {provisionError || loadError ? (
        <p className="mt-1.5 text-xs text-red-700 dark:text-red-300" role="alert">
          {provisionError ? (
            provisionError.upgrade ? (
              <CoreLimitNotice message={provisionError.message} />
            ) : (
              provisionError.message
            )
          ) : (
            loadError
          )}
        </p>
      ) : null}
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <select
          value={role}
          onChange={(e) => setRole(e.target.value as "read_write" | "read_only")}
          className="h-8 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute role"
        >
          <option value="read_write" disabled={hasReadWrite}>
            read-write
          </option>
          <option value="read_only">read-only</option>
        </select>
        <input
          value={cpu}
          onChange={(e) => budget.setCpu(e.target.value)}
          className="h-8 w-16 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute CPU"
          placeholder="CPU"
          title="CPU as a Kubernetes quantity (e.g. 2, 1500m)."
        />
        <input
          value={memory}
          onChange={(e) => budget.setMemory(e.target.value)}
          className="h-8 w-20 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute memory"
          placeholder="Memory"
          title="Memory as a Kubernetes quantity (e.g. 4Gi, 512Mi)."
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-8"
          onClick={() => void provision()}
          // !budget.valid also blocks a blank CPU (server counts "" as 2 cores,
          // which would 402 a gated user); isLoading holds it during the budget-
          // query window so an at-cap user can't slip a default-size provision in.
          disabled={createMut.isPending || !budget.valid || budget.isLoading}
          title={budget.overBudget ? "Exceeds your plan's remaining vCPU" : undefined}
        >
          {createMut.isPending ? "Provisioning…" : "Provision compute"}
        </Button>
      </div>
      {budget.overBudget ? (
        <p className="mt-1 text-[10px] text-red-700 dark:text-red-300" role="alert">
          {overBudgetMessage(budget.remaining)}
        </p>
      ) : null}
      {hasReadWrite ? (
        <p className="mt-1 text-[10px] text-muted-foreground">
          This branch already has a read-write compute, so additional compute must be read-only.
        </p>
      ) : null}
    </div>
  );
}
