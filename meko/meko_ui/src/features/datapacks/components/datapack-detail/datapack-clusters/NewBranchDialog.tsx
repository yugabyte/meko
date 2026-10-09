// Dialog to fork a new branch: pick a parent, name it, optionally pin a
// fork-point LSN, then POST via createBranch.

import { useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { AmpBranch } from "@/lib/api/ampApi";
import { useCreateBranch } from "@/features/datapacks/hooks/useAmpClusters";

export function NewBranchDialog({
  open,
  onOpenChange,
  datapackId,
  clusterId,
  branches,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
  clusterId: string;
  branches: AmpBranch[];
}) {
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState("");
  const [forkLsn, setForkLsn] = useState("");
  const [error, setError] = useState<string | null>(null);
  const createMut = useCreateBranch(datapackId, clusterId);

  useEffect(() => {
    if (open) {
      const root = branches.find((b) => b.is_default) ?? branches[0];
      setName("");
      setParentId(root?.branch_id ?? "");
      setForkLsn("");
      setError(null);
    }
  }, [open, branches]);

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!trimmed || !parentId || createMut.isPending) return;
    setError(null);
    try {
      await createMut.mutateAsync({
        name: trimmed,
        parent_branch_id: parentId,
        fork_point_lsn: forkLsn,
      });
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the branch."));
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md bg-card border-border text-foreground">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void handleCreate();
          }}
        >
          <DialogHeader>
            <DialogTitle className="text-xl text-foreground">New branch</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              Forks a zero-copy clone from the parent branch. A fork point LSN is required when the
              parent has no running compute.
            </DialogDescription>
          </DialogHeader>

          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="amp-branch-parent">Fork from</Label>
              <select
                id="amp-branch-parent"
                value={parentId}
                onChange={(e) => setParentId(e.target.value)}
                className="h-9 w-full rounded-md border border-border bg-background px-2 text-sm text-foreground"
              >
                {branches.map((b) => (
                  <option key={b.branch_id} value={b.branch_id}>
                    {b.name}
                    {b.is_default ? " (default)" : ""}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="amp-branch-name">Branch name</Label>
              <Input
                id="amp-branch-name"
                placeholder="e.g. feature-x"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="amp-branch-lsn">Fork point LSN (optional)</Label>
              <Input
                id="amp-branch-lsn"
                placeholder="e.g. 0/16ABCD0 (defaults to latest)"
                value={forkLsn}
                onChange={(e) => setForkLsn(e.target.value)}
              />
            </div>
          </div>

          {error ? (
            <p className="text-sm text-red-700 dark:text-red-300" role="alert">
              {error}
            </p>
          ) : null}

          <DialogFooter className="gap-2 sm:gap-0">
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={createMut.isPending}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={createMut.isPending || !name.trim() || !parentId}>
              {createMut.isPending ? "Creating…" : "Create branch"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
