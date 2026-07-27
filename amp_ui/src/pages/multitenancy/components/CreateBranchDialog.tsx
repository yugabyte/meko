// Dialog to fork a new sub-branch: pick a parent branch and name, optionally
// pin a fork-point LSN, then POST via the create-branch mutation.

import { useEffect, useState } from "react";
import { toast } from "sonner";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { useCreateBranch } from "@/lib/api/queries";
import type { Branch, CreateBranchInput } from "@/lib/api/types";
import { dataBranchingMessages as msg } from "@/lib/messages";

export function CreateBranchDialog({
  open,
  onOpenChange,
  poolId,
  projectId,
  branches,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  projectId: string;
  branches: Branch[];
}) {
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState<string>("");
  const [forkLsn, setForkLsn] = useState("");
  const createBranch = useCreateBranch();

  // Default the parent to the source branch when the dialog opens.
  useEffect(() => {
    if (open) {
      const root = branches.find((b) => b.is_default) ?? branches[0];
      setParentId(root?.branch_id ?? "");
      setForkLsn("");
    }
  }, [open, branches]);

  const parent = branches.find((b) => b.branch_id === parentId);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed || !parent) return;
    // Controller's CreateBranchRequest only accepts name / parent_branch_id /
    // fork_point_lsn. Send the LSN only when given; otherwise the controller
    // reads the parent's latest LSN — which 400s if the parent has no running
    // endpoint, so the field is offered for that case.
    const body: CreateBranchInput = {
      name: trimmed,
      parent_branch_id: parent.branch_id,
    };
    const lsn = forkLsn.trim();
    if (lsn) body.fork_point_lsn = lsn;

    createBranch.mutate(
      { poolId, projectId, body },
      {
        onSuccess: (branch) => {
          toast.success(msg.branchCreated(branch.name));
          setName("");
          setForkLsn("");
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : msg.branchCreateError,
          ),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>New sub-branch</DialogTitle>
            <DialogDescription>
              Forks a zero-copy clone from the parent branch at its current LSN.
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="branch-parent">Fork from</Label>
              <Select value={parentId} onValueChange={setParentId}>
                <SelectTrigger id="branch-parent">
                  <SelectValue placeholder="Select a parent branch" />
                </SelectTrigger>
                <SelectContent>
                  {branches.map((b) => (
                    <SelectItem key={b.branch_id} value={b.branch_id}>
                      {b.name}
                      {b.is_default ? " (default)" : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="branch-name">Branch name</Label>
              <Input
                id="branch-name"
                placeholder="e.g. agent_eval_a"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="branch-lsn">Fork point LSN (optional)</Label>
              <Input
                id="branch-lsn"
                placeholder="e.g. 0/16ABCD0 — defaults to latest"
                value={forkLsn}
                onChange={(e) => setForkLsn(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                Required when the parent branch has no running endpoint to read
                its latest LSN from.
              </p>
            </div>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={!name.trim() || !parent || createBranch.isPending}
              variant="brand"
            >
              {createBranch.isPending ? "Creating…" : "Create branch"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
