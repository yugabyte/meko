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
import { createDatabase } from "@/lib/api/ampApi";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { toast } from "@/hooks/use-toast";

export type CreateDatabaseDialogProps = {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
  /** Called after a database is created so the caller can refresh its list. */
  onCreated: () => void;
};

/**
 * Spins up an AMP Postgres database inside this datapack. The first database
 * also provisions the datapack's shared pool (handled server-side). Compute to
 * connect is provisioned separately, from the database's detail view.
 */
export function CreateDatabaseDialog({
  open,
  onOpenChange,
  datapackId,
  onCreated,
}: CreateDatabaseDialogProps) {
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
    if (!trimmed) return;
    setSubmitting(true);
    setError(null);
    try {
      const db = await createDatabase(datapackId, { name: trimmed });
      toast({ title: `Database "${db.name}" created` });
      onCreated();
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the database."));
    } finally {
      setSubmitting(false);
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
            <DialogTitle className="text-xl text-foreground">Create database</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              Creates a Postgres database in this datapack. You can provision
              compute to connect to it afterwards. Runs Postgres 17.
            </DialogDescription>
          </DialogHeader>

          <div className="my-4 space-y-2">
            <Label htmlFor="amp-database-name">Database name</Label>
            <Input
              id="amp-database-name"
              autoFocus
              placeholder="e.g. orders"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
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
              disabled={submitting}
            >
              Cancel
            </Button>
            <Button type="submit" disabled={submitting || !name.trim()}>
              {submitting ? "Creating…" : "Create database"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
