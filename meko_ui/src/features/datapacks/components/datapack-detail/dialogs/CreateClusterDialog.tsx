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
import { createCluster } from "@/lib/api/ampApi";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";

// Postgres 18 is the only version AMP supports today, so there's no version
// choice: we omit pg_version on create and the controller applies its default
// (18). If AMP supports multiple versions again, reintroduce a selector here.

export type CreateClusterDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
  /** Called after a cluster is created so the caller can refresh its list. */
  onCreated: () => void;
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
  onCreated,
}: CreateClusterDialogProps) {
  const [name, setName] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setName("");
      setError(null);
      setSubmitting(false);
    }
  }, [open]);

  const handleCreate = async () => {
    const trimmed = name.trim();
    // Guard against double-submit (e.g. repeated Enter) while a request is in
    // flight — belt-and-suspenders with the disabled submit button.
    if (!trimmed || submitting) return;
    setSubmitting(true);
    setError(null);
    try {
      const cluster = await createCluster(datapackId, { name: trimmed });
      toast({ title: `Cluster "${cluster.name}" created` });
      onCreated();
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the cluster."));
    } finally {
      setSubmitting(false);
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
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={submitting || !name.trim()}>
              {submitting ? "Creating…" : "Create cluster"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
