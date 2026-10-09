// A single cluster in the panel list: a collapsible row that expands to show
// the cluster's branch tree, with a type-to-confirm delete.

import { useState } from "react";
import { ChevronDown, ChevronRight, Database, Trash2 } from "lucide-react";
import type { AmpCluster } from "@/lib/api/ampApi";
import { ConfirmByTypingDialog } from "@/components/ui/confirm-by-typing-dialog";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";
import { useDeleteCluster } from "@/features/datapacks/hooks/useAmpClusters";
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
  const [confirmOpen, setConfirmOpen] = useState(false);
  const deleteMut = useDeleteCluster(datapackId);

  const handleDelete = async () => {
    try {
      // Mutation invalidates the clusters query, so the list refreshes itself.
      await deleteMut.mutateAsync({ clusterId: db.id });
      toast({ title: `Cluster "${db.name}" deleted` });
      setConfirmOpen(false);
    } catch (e) {
      toast({ title: getApiErrorMessage(e, "Failed to delete cluster."), variant: "destructive" });
    }
  };

  return (
    <li className="rounded-lg border border-border bg-card">
      <div className="flex items-center">
        <button
          type="button"
          onClick={onToggle}
          className="flex min-w-0 flex-1 items-center gap-2 px-3 py-2.5 text-left"
          aria-expanded={expanded}
        >
          {expanded ? (
            <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
          ) : (
            <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
          )}
          <Database className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
          <span
            className="min-w-0 flex-1 truncate text-sm font-medium text-foreground"
            title={db.name}
          >
            {db.name}
          </span>
          <span className="shrink-0 rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
            pg {db.default_branch.pg_version}
          </span>
        </button>
        <button
          type="button"
          onClick={() => setConfirmOpen(true)}
          className="mr-2 shrink-0 rounded-md p-1.5 text-muted-foreground transition-colors hover:bg-destructive/10 hover:text-destructive"
          aria-label={`Delete cluster ${db.name}`}
          title="Delete cluster"
        >
          <Trash2 className="h-4 w-4" aria-hidden />
        </button>
      </div>
      {expanded ? (
        <div className="border-t border-border px-3 py-3">
          <ClusterBranches datapackId={datapackId} clusterId={db.id} />
        </div>
      ) : null}

      <ConfirmByTypingDialog
        open={confirmOpen}
        onOpenChange={(o) => !deleteMut.isPending && setConfirmOpen(o)}
        title={
          <>
            Delete cluster <span className="font-mono">{db.name}</span>?
          </>
        }
        description={
          <>
            This permanently deletes the cluster and cascades to all its branches and compute — it
            cannot be undone. Type <span className="font-mono font-semibold">{db.name}</span> to
            confirm.
          </>
        }
        confirmPhrase={db.name}
        confirmLabel="Delete cluster"
        pending={deleteMut.isPending}
        onConfirm={handleDelete}
      />
    </li>
  );
}
