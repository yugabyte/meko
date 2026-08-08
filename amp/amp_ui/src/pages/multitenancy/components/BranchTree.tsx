// The branch tree for a cluster: the default branch as source/root, every other
// branch rendered as a fork below it, each with its compute loaded.

import { useMemo } from "react";
import { useBranchEndpoints } from "@/hooks/useEndpoints";
import type { Branch } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";
import { BranchCard } from "./BranchCard";

export function BranchTree({
  poolId,
  clusterId,
  branches,
}: {
  poolId: string;
  clusterId: string;
  branches: Branch[];
}) {
  // Default branch is the source/root; everything else is a fork shown below.
  const root = branches.find((b) => b.is_default);
  const forks = branches.filter((b) => !b.is_default);
  const nameById = useMemo(() => {
    const map: Record<string, string> = {};
    for (const b of branches) map[b.branch_id] = b.name;
    return map;
  }, [branches]);

  const endpoints = useBranchEndpoints(poolId, clusterId, branches);

  if (branches.length === 0) {
    return (
      <p className="py-6 text-center text-sm text-muted-foreground">
        {msg.noBranches}
      </p>
    );
  }

  const ordered = root ? [root, ...forks] : forks;
  return (
    <div className="space-y-3">
      {ordered.map((branch) => (
        <BranchCard
          key={branch.branch_id}
          branch={branch}
          isRoot={branch.is_default}
          parentName={
            branch.parent_branch_id
              ? nameById[branch.parent_branch_id]
              : undefined
          }
          poolId={poolId}
          clusterId={clusterId}
          endpoints={endpoints.byBranch[branch.branch_id] ?? []}
          endpointsLoading={endpoints.isLoading}
        />
      ))}
    </div>
  );
}
