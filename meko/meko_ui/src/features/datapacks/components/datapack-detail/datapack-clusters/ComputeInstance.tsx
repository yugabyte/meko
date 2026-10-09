// One provisioned compute instance on a branch: address + role + resource tag,
// an in-place resize panel, unprovision (forked branches only), and a copyable
// psql connect string. Mirrors AMP's endpoint card using Meko primitives.

import { useState } from "react";
import { Check, Copy, Cpu, MemoryStick, Server, Trash2 } from "lucide-react";
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
import { copyToClipboard } from "@/lib/clipboard";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { ampCoreLimitMessage } from "@/lib/api/ampApi";
import { CoreLimitNotice } from "./CoreLimitNotice";
import type { AmpCompute } from "@/lib/api/ampApi";
import { useDeleteCompute, useResizeCompute } from "@/features/datapacks/hooks/useAmpClusters";
import { resizeToast } from "./resize";

export function ComputeInstance({
  ep,
  datapackId,
  clusterId,
  branchId,
  branchName,
  isDefaultBranch,
}: {
  ep: AmpCompute;
  datapackId: string;
  clusterId: string;
  branchId: string;
  branchName: string;
  isDefaultBranch: boolean;
}) {
  const [copied, setCopied] = useState(false);
  const psql = ep.address ? `psql "postgresql://cloud_admin@${ep.address}/postgres"` : "";

  // Per-endpoint in-place resize, pre-filled with the current reservation.
  const [showResize, setShowResize] = useState(false);
  const [cpu, setCpu] = useState(ep.resources?.cpu ?? "");
  const [memory, setMemory] = useState(ep.resources?.memory ?? "");
  const resizeMut = useResizeCompute(datapackId, clusterId, branchId);

  // Unprovision (delete) — offered on forked branches only; the source (default)
  // branch's compute can't be torn down from the UI for now.
  const [confirmDelete, setConfirmDelete] = useState(false);
  const deleteMut = useDeleteCompute(datapackId, clusterId, branchId);
  const roleLabel = ep.role.replace("_", "-");

  // Both mutations invalidate the branch's compute query, so the list refetches
  // on success — no manual onChanged callback needed.
  const handleCopy = async () => {
    if (!psql) return;
    const okCopy = await copyToClipboard(psql);
    if (!okCopy) return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };

  const applyResize = async () => {
    if ((!cpu.trim() && !memory.trim()) || resizeMut.isPending) return;
    try {
      const res = await resizeMut.mutateAsync({ computeId: ep.id, resources: { cpu, memory } });
      const size = `${res.endpoint.resources?.cpu ?? cpu} CPU / ${res.endpoint.resources?.memory ?? memory}`;
      toast(resizeToast(res.outcome, size));
      setShowResize(false);
    } catch (e) {
      const upgradeMsg = ampCoreLimitMessage(e);
      toast({
        title: "Resize failed",
        description: upgradeMsg ? (
          <CoreLimitNotice message={upgradeMsg} />
        ) : (
          getApiErrorMessage(e, "Failed to resize compute.")
        ),
        variant: "destructive",
      });
    }
  };

  const unprovision = async () => {
    if (deleteMut.isPending) return;
    try {
      await deleteMut.mutateAsync({ computeId: ep.id });
      toast({ title: `Unprovisioned ${roleLabel} compute` });
      setConfirmDelete(false);
    } catch (e) {
      toast({
        title: "Unprovision failed",
        description: getApiErrorMessage(e, "Failed to unprovision compute."),
        variant: "destructive",
      });
    }
  };

  const hasResources = Boolean(ep.resources?.cpu || ep.resources?.memory);

  return (
    <li className="rounded-md border border-border bg-card p-2">
      <div className="flex items-start gap-2">
        <Server className="mt-0.5 h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <div className="min-w-0 flex-1 space-y-0.5">
          {/* primary (public) address + role badge; matches AMP's endpoint card. */}
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="w-12 shrink-0 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
              primary
            </span>
            <span className="min-w-0 break-all font-mono text-xs">
              {ep.address || "provisioning…"}
            </span>
            <span className="rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
              {roleLabel}
            </span>
            {/* current compute reservation (cpu / memory), when the controller reports it. */}
            {hasResources ? (
              <span className="inline-flex items-center gap-1 rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                <Cpu className="h-3 w-3" aria-hidden />
                {ep.resources?.cpu ?? "n/a"}
                <MemoryStick className="ml-1 h-3 w-3" aria-hidden />
                {ep.resources?.memory ?? "n/a"}
              </span>
            ) : null}
          </div>
          {/* tailnet address — only when AMP exposed the endpoint on the tailnet. */}
          {ep.tailnet_address ? (
            <div className="flex flex-wrap items-center gap-x-2">
              <span className="w-12 shrink-0 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">
                tailnet
              </span>
              <span className="min-w-0 break-all font-mono text-xs text-muted-foreground">
                {ep.tailnet_address}
              </span>
            </div>
          ) : null}
        </div>
        <div className="flex shrink-0 items-center gap-1">
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="h-7 gap-1 px-2 text-[11px]"
            onClick={() => setShowResize((s) => !s)}
          >
            <Cpu className="h-3.5 w-3.5" aria-hidden />
            Resize
          </Button>
          {!isDefaultBranch ? (
            <Button
              type="button"
              variant="ghost"
              size="sm"
              className="h-7 gap-1 px-2 text-[11px] text-destructive hover:text-destructive"
              onClick={() => setConfirmDelete(true)}
              disabled={deleteMut.isPending}
            >
              <Trash2 className="h-3.5 w-3.5" aria-hidden />
              {deleteMut.isPending ? "Unprovisioning…" : "Unprovision"}
            </Button>
          ) : null}
        </div>
      </div>

      {showResize ? (
        <div className="mt-1.5 flex flex-wrap items-center gap-2 rounded-md border border-border bg-muted/30 p-2">
          <input
            value={cpu}
            onChange={(e) => setCpu(e.target.value)}
            className="h-8 w-16 rounded-md border border-border bg-background px-2 text-xs text-foreground"
            aria-label="Resize CPU"
            placeholder="unchanged"
            title="CPU as a Kubernetes quantity (e.g. 4, 1500m). Blank = leave unchanged."
          />
          <input
            value={memory}
            onChange={(e) => setMemory(e.target.value)}
            className="h-8 w-20 rounded-md border border-border bg-background px-2 text-xs text-foreground"
            aria-label="Resize memory"
            placeholder="unchanged"
            title="Memory as a Kubernetes quantity (e.g. 8Gi, 512Mi). Blank = leave unchanged."
          />
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-8"
            onClick={() => void applyResize()}
            disabled={resizeMut.isPending || (!cpu.trim() && !memory.trim())}
          >
            {resizeMut.isPending ? "Resizing…" : "Apply resize"}
          </Button>
        </div>
      ) : null}

      {ep.address ? (
        <div className="mt-1.5 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-[11px] text-muted-foreground">
            {psql}
          </code>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 shrink-0 gap-1"
            onClick={() => void handleCopy()}
            aria-label="Copy psql command"
          >
            {copied ? (
              <Check className="h-3.5 w-3.5" aria-hidden />
            ) : (
              <Copy className="h-3.5 w-3.5" aria-hidden />
            )}
          </Button>
        </div>
      ) : null}

      <AlertDialog
        open={confirmDelete}
        onOpenChange={(o) => !o && !deleteMut.isPending && setConfirmDelete(false)}
      >
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle>
              Unprovision this {roleLabel} compute on{" "}
              <span className="font-mono">{branchName}</span>?
            </AlertDialogTitle>
            <AlertDialogDescription>
              This tears down the running compute instance on branch{" "}
              <span className="font-mono">{branchName}</span>. The branch and its data are
              untouched. You can re-provision compute later.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel disabled={deleteMut.isPending}>Cancel</AlertDialogCancel>
            <AlertDialogAction
              disabled={deleteMut.isPending}
              onClick={(e) => {
                e.preventDefault();
                void unprovision();
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
