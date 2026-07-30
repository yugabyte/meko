// The Compute section under a branch: lists provisioned compute and the
// controls to provision another. Enforces AMP's one-read-write-per-branch rule.

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { useCompute, useCreateCompute } from "@/features/datapacks/hooks/useAmpClusters";
import { ComputeInstance } from "./ComputeInstance";

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
  // CPU / memory as Kubernetes quantities, mirroring AMP's endpoint `resources`
  // (prefilled with AMP's defaults so the user sees + can override them).
  const [cpu, setCpu] = useState("2");
  const [memory, setMemory] = useState("4Gi");
  const [provisionError, setProvisionError] = useState<string | null>(null);
  const error =
    provisionError ??
    (computeQuery.error ? getApiErrorMessage(computeQuery.error, "Failed to load compute.") : null);

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
      setProvisionError(getApiErrorMessage(e, "Failed to provision compute."));
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
      {error ? (
        <p className="mt-1.5 text-xs text-red-700 dark:text-red-300" role="alert">
          {error}
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
          onChange={(e) => setCpu(e.target.value)}
          className="h-8 w-16 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute CPU"
          placeholder="CPU"
          title="CPU as a Kubernetes quantity (e.g. 2, 1500m). Blank = AMP default (2)."
        />
        <input
          value={memory}
          onChange={(e) => setMemory(e.target.value)}
          className="h-8 w-20 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute memory"
          placeholder="Memory"
          title="Memory as a Kubernetes quantity (e.g. 4Gi, 512Mi). Blank = AMP default (4Gi)."
        />
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-8"
          onClick={() => void provision()}
          disabled={createMut.isPending}
        >
          {createMut.isPending ? "Provisioning…" : "Provision compute"}
        </Button>
      </div>
      {hasReadWrite ? (
        <p className="mt-1 text-[10px] text-muted-foreground">
          This branch already has a read-write compute, so additional compute must be read-only.
        </p>
      ) : null}
    </div>
  );
}
