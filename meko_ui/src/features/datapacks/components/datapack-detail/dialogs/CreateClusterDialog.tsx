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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { createCluster } from "@/lib/api/ampApi";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";

// Postgres major versions offered when creating a cluster. AMP's supported set
// has been shifting (storage controller vs controller), so this is selectable
// and defaults to the currently-supported 18 rather than hardcoding one value.
const PG_VERSIONS = ["18", "17"] as const;
const DEFAULT_PG_VERSION = "18";

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
  const [pgVersion, setPgVersion] = useState<string>(DEFAULT_PG_VERSION);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setName("");
      setPgVersion(DEFAULT_PG_VERSION);
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
      const cluster = await createCluster(datapackId, {
        name: trimmed,
        pg_version: Number(pgVersion),
      });
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

            <div className="space-y-2">
              <Label htmlFor="amp-pg-version">Postgres version</Label>
              <Select value={pgVersion} onValueChange={setPgVersion}>
                <SelectTrigger id="amp-pg-version">
                  <SelectValue />
                </SelectTrigger>
                <SelectContent>
                  {PG_VERSIONS.map((v) => (
                    <SelectItem key={v} value={v}>
                      Postgres {v}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
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
