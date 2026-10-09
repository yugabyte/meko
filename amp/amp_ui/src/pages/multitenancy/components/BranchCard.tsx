// One branch in the tree: its identity badges (name, source/fork, pg version,
// lineage), the extensions enabled on it, plus the compute section for that
// branch, and — on forks — a delete.

import { useState } from "react";
import { Blocks, GitBranch, SlidersHorizontal, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import { ApiError } from "@/lib/api/client";
import { useDeleteBranch } from "@/lib/api/queries";
import type { Branch, Endpoint } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";
import { formatDateTime } from "@/lib/format";
import { EndpointsSection } from "./EndpointsSection";
import { ExtensionsDialog } from "./ExtensionsDialog";
import { SettingsDialog } from "./SettingsDialog";

export function BranchCard({
  branch,
  isRoot,
  parentName,
  poolId,
  clusterId,
  endpoints,
  endpointsLoading,
}: {
  branch: Branch;
  isRoot: boolean;
  parentName?: string;
  poolId: string;
  clusterId: string;
  endpoints: Endpoint[];
  endpointsLoading: boolean;
}) {
  // Delete is offered on forks only — the source (default) branch is removed by
  // deleting the cluster, not on its own. Plain AlertDialog (like unprovision):
  // a fork's data is real but it's a fork, not the whole cluster.
  const del = useDeleteBranch();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [showSettings, setShowSettings] = useState(false);
  const [showExtensions, setShowExtensions] = useState(false);
  const extensions = branch.extensions ?? [];

  function remove() {
    del.mutate(
      { poolId, clusterId, branchId: branch.branch_id },
      {
        onSuccess: () => {
          toast.success(msg.deleteBranchSuccess(branch.name));
          setConfirmDelete(false);
        },
        onError: (err) => {
          // 409 = the branch has sub-branches forked off it; tell the user to
          // delete those first rather than showing a generic failure.
          if (err instanceof ApiError && err.status === 409) {
            toast.error(msg.deleteBranchHasChildren);
          } else {
            toast.error(err instanceof Error ? err.message : msg.deleteBranchError);
          }
        },
      },
    );
  }

  return (
    <div className={cn("rounded-lg border border-border bg-background/40 p-4", !isRoot && "ml-6")}>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge variant="outline" className="gap-1 font-mono">
          <GitBranch className="h-3 w-3" />
          {branch.name}
        </Badge>
        {isRoot ? (
          <Badge className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">source</Badge>
        ) : (
          <Badge className="bg-teal-500/15 text-teal-600 dark:text-teal-400">fork</Badge>
        )}
        <span className="font-mono text-xs text-muted-foreground">pg {branch.pg_version}</span>
        <span className="font-mono text-xs text-muted-foreground">{branch.branch_id}</span>
        {parentName && (
          <span className="text-xs text-muted-foreground">
            forked from <span className="font-medium text-foreground">{parentName}</span>
            {branch.fork_lsn && (
              <>
                {" "}
                @ <span className="font-mono">{branch.fork_lsn}</span>
              </>
            )}
          </span>
        )}
        <div className="ml-auto flex items-center gap-2">
          <span className="text-xs text-muted-foreground">{formatDateTime(branch.created_at)}</span>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setShowExtensions(true)}
            className="h-7 gap-1 px-2 text-xs"
          >
            <Blocks className="h-3.5 w-3.5" />
            Extensions
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setShowSettings(true)}
            className="h-7 gap-1 px-2 text-xs"
          >
            <SlidersHorizontal className="h-3.5 w-3.5" />
            Settings
          </Button>
          {!isRoot && (
            <Button
              variant="ghost"
              size="sm"
              onClick={() => setConfirmDelete(true)}
              disabled={del.isPending}
              className="h-7 gap-1 px-2 text-xs text-destructive hover:text-destructive"
            >
              <Trash2 className="h-3.5 w-3.5" />
              {del.isPending ? "Deleting…" : "Delete"}
            </Button>
          )}
        </div>
      </div>

      {extensions.length > 0 && (
        <div className="mt-2 flex flex-wrap items-center gap-1.5">
          <span className="text-xs text-muted-foreground">Extensions</span>
          {extensions.map((name) => (
            <Badge key={name} variant="outline" className="font-mono text-[10px]">
              {name}
            </Badge>
          ))}
        </div>
      )}

      <EndpointsSection
        poolId={poolId}
        clusterId={clusterId}
        branchId={branch.branch_id}
        endpoints={endpoints}
        loading={endpointsLoading}
        isDefaultBranch={isRoot}
      />

      <AlertDialog
        open={confirmDelete}
        onOpenChange={(o) => !o && !del.isPending && setConfirmDelete(false)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{msg.deleteBranchTitle(branch.name)}</AlertDialogTitle>
            <AlertDialogDescription>{msg.deleteBranchConfirm}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={del.isPending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={del.isPending}
              onClick={(e) => {
                e.preventDefault();
                remove();
              }}
            >
              {del.isPending ? "Deleting…" : "Delete branch"}
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <ExtensionsDialog
        open={showExtensions}
        onOpenChange={setShowExtensions}
        poolId={poolId}
        clusterId={clusterId}
        branchId={branch.branch_id}
        enabled={extensions}
        title={branch.name}
      />
      <SettingsDialog
        open={showSettings}
        onOpenChange={setShowSettings}
        poolId={poolId}
        clusterId={clusterId}
        branchId={branch.branch_id}
        stored={{
          pg_settings: branch.pg_settings,
          write_token_settings: branch.write_token_settings,
        }}
        title={branch.name}
      />
    </div>
  );
}
