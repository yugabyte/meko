// One branch in the tree: its identity badges (name, source/fork, pg version,
// lineage) plus the compute section for that branch.

import { GitBranch } from "lucide-react";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import type { Branch, Endpoint } from "@/lib/api/types";
import { formatDateTime, shortId } from "@/lib/format";
import { EndpointsSection } from "./EndpointsSection";

export function BranchCard({
  branch,
  isRoot,
  parentName,
  poolId,
  projectId,
  endpoints,
  endpointsLoading,
}: {
  branch: Branch;
  isRoot: boolean;
  parentName?: string;
  poolId: string;
  projectId: string;
  endpoints: Endpoint[];
  endpointsLoading: boolean;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border border-border bg-background/40 p-4",
        !isRoot && "ml-6",
      )}
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge variant="outline" className="gap-1 font-mono">
          <GitBranch className="h-3 w-3" />
          {branch.name}
        </Badge>
        {isRoot ? (
          <Badge className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">
            source
          </Badge>
        ) : (
          <Badge className="bg-teal-500/15 text-teal-600 dark:text-teal-400">
            fork
          </Badge>
        )}
        <span className="font-mono text-xs text-muted-foreground">
          pg {branch.pg_version}
        </span>
        <span className="font-mono text-xs text-muted-foreground">
          {shortId(branch.branch_id)}
        </span>
        {parentName && (
          <span className="text-xs text-muted-foreground">
            forked from{" "}
            <span className="font-medium text-foreground">{parentName}</span>
            {branch.fork_lsn && (
              <>
                {" "}
                @ <span className="font-mono">{branch.fork_lsn}</span>
              </>
            )}
          </span>
        )}
        <span className="ml-auto text-xs text-muted-foreground">
          {formatDateTime(branch.created_at)}
        </span>
      </div>

      <EndpointsSection
        poolId={poolId}
        projectId={projectId}
        branchId={branch.branch_id}
        endpoints={endpoints}
        loading={endpointsLoading}
        isDefaultBranch={isRoot}
      />
    </div>
  );
}
