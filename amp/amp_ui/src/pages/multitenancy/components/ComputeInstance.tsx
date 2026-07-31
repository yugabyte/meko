// One provisioned compute instance under a branch: address, role, resource tag,
// in-place resize panel, unprovision confirm, and a copyable psql connect string.

import { useState } from "react";
import { Check, Copy, Cpu, Server, Trash2 } from "lucide-react";
import { toast } from "sonner";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
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
import { useDeleteEndpoint, useResizeEndpoint } from "@/hooks/useEndpoints";
import type { Endpoint } from "@/lib/api/types";
import { DEFAULT_ENDPOINT_CPU, DEFAULT_ENDPOINT_MEMORY } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";
import { ComputeResourceTag } from "@/components/ResourceTag";

// The controller returns `address` (host:port) once the instance is deployed; it
// connects as user `cloud_admin`, cluster `postgres`.
export function ComputeInstance({
  ep,
  poolId,
  isDefaultBranch,
}: {
  ep: Endpoint;
  poolId: string;
  isDefaultBranch: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const connStr = ep.address
    ? `postgresql://cloud_admin@${ep.address}/postgres`
    : "";
  const psql = connStr ? `psql "${connStr}"` : "";

  // Per-endpoint in-place resize: pre-fill with the current reservation. The
  // controller persists the new size, updates the Deployment, and resizes the
  // running pod in place; the reported outcome says whether it applied live.
  const resize = useResizeEndpoint();
  const [showResize, setShowResize] = useState(false);
  const [cpu, setCpu] = useState(ep.resources?.cpu || DEFAULT_ENDPOINT_CPU);
  const [memory, setMemory] = useState(
    ep.resources?.memory || DEFAULT_ENDPOINT_MEMORY,
  );

  // Unprovision (delete) this compute instance. Confirmed via AlertDialog since
  // it tears down the running pod — the branch/data are untouched, only the
  // compute is removed and can be re-provisioned later.
  const del = useDeleteEndpoint();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const roleLabel = ep.role.replace("_", "-");
  const isReadWrite = ep.role === "read_write";

  function unprovision() {
    del.mutate(
      {
        poolId,
        projectId: ep.project_id,
        branchId: ep.branch_id,
        endpointId: ep.id,
      },
      {
        onSuccess: () => {
          toast.success(msg.unprovisionSuccess(roleLabel));
          setConfirmDelete(false);
        },
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : msg.unprovisionError,
          ),
      },
    );
  }

  function applyResize() {
    resize.mutate(
      {
        poolId,
        projectId: ep.project_id,
        branchId: ep.branch_id,
        endpointId: ep.id,
        cpu: cpu.trim() || undefined,
        memory: memory.trim() || undefined,
      },
      {
        onSuccess: (res) => {
          const size = `${res.endpoint.resources?.cpu ?? cpu} CPU / ${res.endpoint.resources?.memory ?? memory}`;
          switch (res.outcome) {
            case "completed":
              toast.success(msg.resizeCompleted(size));
              break;
            case "pending":
              toast.success(msg.resizePending(size));
              break;
            case "infeasible":
              toast.warning(msg.resizeInfeasible(size));
              break;
            case "unsupported":
              toast.success(msg.resizeUnsupported(size));
              break;
          }
          setShowResize(false);
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : msg.resizeError),
      },
    );
  }

  function copy() {
    if (!psql || !navigator.clipboard) return;
    navigator.clipboard
      .writeText(psql)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      })
      .catch((err) => {
        toast.error(msg.clipboardError);
        console.error("Clipboard copy failed:", err);
      });
  }

  return (
    <li className="rounded-md border border-border bg-background/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Server className="h-4 w-4 text-muted-foreground" />
        <span className="flex flex-col gap-0.5 font-mono text-xs">
          <span>
            {ep.tailnet_address && (
              <span className="text-muted-foreground">primary: </span>
            )}
            {ep.address || "provisioning…"}
          </span>
          {ep.tailnet_address && (
            <span>
              <span className="text-muted-foreground">tailnet: </span>
              {ep.tailnet_address}
            </span>
          )}
        </span>
        <Badge variant="secondary" className="font-mono text-[10px]">
          {ep.role.replace("_", "-")}
        </Badge>
        <ComputeResourceTag cpu={ep.resources?.cpu} memory={ep.resources?.memory} />
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setShowResize((s) => !s)}
          className="ml-auto h-7 gap-1 px-2 text-xs"
        >
          <Cpu className="h-3.5 w-3.5" />
          Resize
        </Button>
        {/* Unprovision is offered on forked branches only — the source (default)
            branch's compute can't be torn down from the UI for now. */}
        {!isDefaultBranch && (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setConfirmDelete(true)}
            disabled={del.isPending}
            className="h-7 gap-1 px-2 text-xs text-destructive hover:text-destructive"
          >
            <Trash2 className="h-3.5 w-3.5" />
            {del.isPending ? "Unprovisioning…" : "Unprovision"}
          </Button>
        )}
      </div>
      {showResize && (
        <div className="mt-2 flex flex-wrap items-end gap-2 rounded-md border border-border bg-muted/30 p-2">
          <div className="flex flex-col gap-1">
            <Label className="text-[10px] uppercase tracking-wide text-muted-foreground">
              CPU
            </Label>
            <Input
              value={cpu}
              onChange={(e) => setCpu(e.target.value)}
              placeholder={DEFAULT_ENDPOINT_CPU}
              className="h-8 w-20 font-mono text-xs"
            />
          </div>
          <div className="flex flex-col gap-1">
            <Label className="text-[10px] uppercase tracking-wide text-muted-foreground">
              Memory
            </Label>
            <Input
              value={memory}
              onChange={(e) => setMemory(e.target.value)}
              placeholder={DEFAULT_ENDPOINT_MEMORY}
              className="h-8 w-24 font-mono text-xs"
            />
          </div>
          <Button
            variant="outline"
            size="sm"
            onClick={applyResize}
            disabled={resize.isPending}
          >
            {resize.isPending ? "Resizing…" : "Apply resize"}
          </Button>
        </div>
      )}
      {ep.address && (
        <div className="mt-2 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground">
            {psql}
          </code>
          <Button
            variant="outline"
            size="sm"
            onClick={copy}
            className="h-7 shrink-0 gap-1"
          >
            {copied ? (
              <Check className="h-3.5 w-3.5" />
            ) : (
              <Copy className="h-3.5 w-3.5" />
            )}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
      )}

      <AlertDialog
        open={confirmDelete}
        onOpenChange={(o) => !o && !del.isPending && setConfirmDelete(false)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>{msg.unprovisionTitle(roleLabel)}</AlertDialogTitle>
            <AlertDialogDescription>
              {isReadWrite
                ? msg.unprovisionConfirmReadWrite
                : msg.unprovisionConfirmReadOnly}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={del.isPending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={del.isPending}
              onClick={(e) => {
                e.preventDefault();
                unprovision();
              }}
            >
              Unprovision
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
    </li>
  );
}
