// One branch in a cluster's tree: identity badges (name, source/fork, pg
// version, fork LSN) plus its compute section.

import { GitBranch } from "lucide-react";
import { cn } from "@/lib/utils";
import type { AmpBranch } from "@/lib/api/ampApi";
import { BranchCompute } from "./BranchCompute";

export function BranchCard({
  datapackId,
  clusterId,
  branch,
}: {
  datapackId: string;
  clusterId: string;
  branch: AmpBranch;
}) {
  return (
    <div className="rounded-md border border-border bg-background/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="inline-flex items-center gap-1 rounded border border-border px-1.5 py-0.5 font-mono text-xs">
          <GitBranch className="h-3 w-3" aria-hidden />
          {branch.name}
        </span>
        <span
          className={cn(
            "rounded px-1.5 py-0.5 text-[10px] font-medium",
            branch.is_default
              ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
              : "bg-teal-500/15 text-teal-600 dark:text-teal-400",
          )}
        >
          {branch.is_default ? "source" : "fork"}
        </span>
        <span className="font-mono text-xs text-muted-foreground">pg {branch.pg_version}</span>
        {branch.fork_lsn ? (
          <span className="font-mono text-xs text-muted-foreground">@ {branch.fork_lsn}</span>
        ) : null}
      </div>
      <BranchCompute
        datapackId={datapackId}
        clusterId={clusterId}
        branchId={branch.branch_id}
        branchName={branch.name}
        isDefaultBranch={branch.is_default}
      />
    </div>
  );
}
