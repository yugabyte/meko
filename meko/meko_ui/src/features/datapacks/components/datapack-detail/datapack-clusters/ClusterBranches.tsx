// A cluster's branch tree: the default branch (source) first, forks below, each
// with its compute. Hosts the "New branch" action + dialog.

import { useMemo, useState } from "react";
import { GitBranch, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { useBranches } from "@/features/datapacks/hooks/useAmpClusters";
import { BranchCard } from "./BranchCard";
import { NewBranchDialog } from "./NewBranchDialog";

export function ClusterBranches({
  datapackId,
  clusterId,
}: {
  datapackId: string;
  clusterId: string;
}) {
  const branchesQuery = useBranches(datapackId, clusterId);
  const branches = useMemo(() => branchesQuery.data ?? [], [branchesQuery.data]);
  const loading = branchesQuery.isLoading;
  const error = branchesQuery.error
    ? getApiErrorMessage(branchesQuery.error, "Failed to load branches.")
    : null;
  const [newBranchOpen, setNewBranchOpen] = useState(false);

  const ordered = useMemo(() => {
    const root = branches.filter((b) => b.is_default);
    const forks = branches.filter((b) => !b.is_default);
    return [...root, ...forks];
  }, [branches]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <GitBranch className="h-4 w-4 shrink-0 text-orange-400" aria-hidden />
        <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Branches
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="ml-auto h-7 gap-1"
          onClick={() => setNewBranchOpen(true)}
          disabled={loading || branches.length === 0}
        >
          <Plus className="h-3.5 w-3.5" aria-hidden />
          New branch
        </Button>
      </div>

      {loading ? (
        <p className="text-xs text-muted-foreground">Loading branches…</p>
      ) : error ? (
        <p className="text-xs text-red-700 dark:text-red-300" role="alert">
          {error}
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {ordered.map((branch) => (
            <BranchCard
              key={branch.branch_id}
              datapackId={datapackId}
              clusterId={clusterId}
              branch={branch}
            />
          ))}
        </div>
      )}

      <NewBranchDialog
        open={newBranchOpen}
        onOpenChange={setNewBranchOpen}
        datapackId={datapackId}
        clusterId={clusterId}
        branches={branches}
      />
    </div>
  );
}
