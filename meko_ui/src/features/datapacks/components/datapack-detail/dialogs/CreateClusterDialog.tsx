import { useEffect, useState } from "react";
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
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";
import { useCreateCluster } from "@/features/datapacks/hooks/useAmpClusters";

// Postgres 18 is the only version AMP supports today, so there's no version
// choice: we omit pg_version on create and the controller applies its default
// (18). If AMP supports multiple versions again, reintroduce a selector here.

export type CreateClusterDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
};

/**
 * Spins up an AMP Postgres cluster inside this datapack. The first cluster
 * also provisions the datapack's shared pool (handled server-side). Compute to
 * connect is provisioned separately, from the cluster's detail view.
 */
export function CreateClusterDialog({
  open,
  onOpenChange,
  datapackId,
}: CreateClusterDialogProps) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  // Mutation invalidates the clusters query on success, so every list surface
  // (panel + tile) refreshes without a manual reload.
  const createMut = useCreateCluster(datapackId);

  useEffect(() => {
    if (open) {
      setName("");
      setError(null);
    }
  }, [open]);

  const handleCreate = async () => {
    const trimmed = name.trim();
    // Guard against double-submit (e.g. repeated Enter) while a request is in
    // flight — belt-and-suspenders with the disabled submit button.
    if (!trimmed || createMut.isPending) return;
    setError(null);
    try {
      const cluster = await createMut.mutateAsync({ name: trimmed });
      toast({ title: `Cluster "${cluster.name}" created` });
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the cluster."));
    }
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-md max-h-[85vh] overflow-y-auto bg-card border-border text-foreground">
        <form
          onSubmit={(e) => {
            e.preventDefault();
            void handleCreate();
          }}
        >
          <DialogHeader>
            <DialogTitle className="text-xl text-foreground">Create cluster</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              Creates a Postgres cluster in this datapack. You can provision
              compute to connect to it afterwards.
            </DialogDescription>
          </DialogHeader>

          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="amp-cluster-name">Cluster name</Label>
              <Input
                id="amp-cluster-name"
                autoFocus
                placeholder="e.g. orders"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
          </div>

          {error ? (
            <div
              role="alert"
              className="mb-4 max-h-32 overflow-y-auto whitespace-pre-wrap break-words rounded-md border border-red-500/30 bg-red-500/10 px-3 py-2 text-sm text-red-700 dark:text-red-300"
            >
              {error}
            </div>
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
            <Button type="submit" disabled={createMut.isPending || !name.trim()}>
              {createMut.isPending ? "Creating…" : "Create cluster"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
