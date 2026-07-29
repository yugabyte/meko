// A single cluster in the panel list: a collapsible row that expands to show
// the cluster's branch tree.

import { ChevronDown, ChevronRight, Database } from "lucide-react";
import type { AmpCluster } from "@/lib/api/ampApi";
import { ClusterBranches } from "./ClusterBranches";

export function ClusterRow({
  datapackId,
  db,
  expanded,
  onToggle,
}: {
  datapackId: string;
  db: AmpCluster;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <li className="rounded-lg border border-border bg-card">
      <button
        type="button"
        onClick={onToggle}
        className="flex w-full items-center gap-2 px-3 py-2.5 text-left"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        ) : (
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        )}
        <Database className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground" title={db.name}>
          {db.name}
        </span>
        <span className="shrink-0 rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
          pg {db.default_branch.pg_version}
        </span>
      </button>
      {expanded ? (
        <div className="border-t border-border px-3 py-3">
          <ClusterBranches datapackId={datapackId} clusterId={db.id} />
        </div>
      ) : null}
    </li>
  );
}
