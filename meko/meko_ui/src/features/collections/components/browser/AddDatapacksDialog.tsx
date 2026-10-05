// Search your datapacks and file several into one folder at once.
//
// Placement is an upsert keyed on the datapack, so "add" and "move" are the
// same write: picking one that already sits in another folder moves it here.
// The list says so rather than hiding those, because moving is usually what
// the user means.

import { useEffect, useMemo, useState } from "react";
import { Database, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { toast } from "@/hooks/use-toast";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import type { Datapack } from "@/lib/api/mekoApi";
import type { CollectionDatapackNode, CollectionFolder } from "@/lib/api/collectionsApi";
import { usePlaceDatapack } from "@/features/collections/hooks/useCollections";
import { addableDatapacks } from "@/features/collections/lib/folderContents";
import {
  ADD_A_DATAPACK_LABEL,
  COLLECTION_TOASTS,
  TOAST_RETRY_DESCRIPTION,
} from "@/features/collections/collectionsCopy";
import { DATAPACK_NAME_FONT } from "@/features/collections/collectionsTypography";
import { cn } from "@/lib/utils";

export function AddDatapacksDialog({
  open,
  onOpenChange,
  folder,
  nodes,
  ownDatapacks,
  pathByDatapack,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  folder: CollectionFolder | undefined;
  nodes: CollectionDatapackNode[];
  /** Owned only: placement refuses anything else. */
  ownDatapacks: Datapack[];
  /** Where each datapack sits today, so a row can say it will be moved. */
  pathByDatapack: Record<string, { path: string } | undefined>;
}) {
  const [query, setQuery] = useState("");
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const placeMut = usePlaceDatapack();

  useEffect(() => {
    if (!open) return;
    setQuery("");
    setPicked(new Set());
    setError(null);
  }, [open, folder?.folder_id]);

  const candidates = useMemo(
    () => (folder ? addableDatapacks(ownDatapacks, nodes, folder.folder_id, query) : []),
    [ownDatapacks, nodes, folder, query],
  );

  const toggle = (id: string) =>
    setPicked((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });

  const handleAdd = async () => {
    if (!folder || picked.size === 0 || busy) return;
    setBusy(true);
    setError(null);

    // One call per datapack: placement is per-datapack, so a batch is N
    // writes. Settle them all rather than stopping at the first failure —
    // a half-applied batch the user cannot see is worse than a partial
    // success they can.
    const ids = Array.from(picked);
    const results = await Promise.allSettled(
      ids.map((datapackId) => placeMut.mutateAsync({ datapackId, folderId: folder.folder_id })),
    );
    const failed = results.filter((r) => r.status === "rejected").length;
    const added = ids.length - failed;
    setBusy(false);

    if (failed === 0) {
      onOpenChange(false);
      toast({
        title: COLLECTION_TOASTS.datapackFiled.ok,
        description: `${added} added to ${folder.folder_name}.`,
      });
      return;
    }
    const first = results.find((r) => r.status === "rejected") as PromiseRejectedResult | undefined;
    const message = getApiErrorMessage(first?.reason, TOAST_RETRY_DESCRIPTION);
    setError(
      added > 0
        ? `${added} added, ${failed} failed. ${message}`
        : message,
    );
    toast({
      variant: "destructive",
      title: COLLECTION_TOASTS.datapackFiled.fail,
      description: message,
    });
  };

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="w-[calc(100vw-2rem)] sm:max-w-lg bg-card border-border text-foreground">
        <DialogHeader>
          <DialogTitle>{ADD_A_DATAPACK_LABEL}</DialogTitle>
          <DialogDescription>
            {folder
              ? `Pick the datapacks that belong in ${folder.folder_name}.`
              : "Pick a folder first."}
          </DialogDescription>
        </DialogHeader>

        <div className="space-y-3 py-2">
          <div className="relative">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder="Search your datapacks..."
              aria-label="Search datapacks to add"
              className="h-9 w-full border-border bg-input pl-9"
              autoFocus
            />
          </div>

          <ul className="max-h-[18rem] min-h-[6rem] space-y-1 overflow-y-auto rounded-lg border border-border p-1">
            {candidates.length === 0 ? (
              <li className="px-2 py-6 text-center text-sm text-muted-foreground">
                {query.trim()
                  ? "No datapacks match."
                  : "Every datapack you own is already in this folder."}
              </li>
            ) : (
              candidates.map((d) => {
                const here = pathByDatapack[d.datapack_id]?.path;
                return (
                  <li key={d.datapack_id}>
                    <label className="flex cursor-pointer items-center gap-3 rounded-md px-2 py-2 transition-colors hover:bg-foreground/5">
                      <Checkbox
                        checked={picked.has(d.datapack_id)}
                        onCheckedChange={() => toggle(d.datapack_id)}
                        aria-label={`Add ${d.datapack_name}`}
                      />
                      <Database className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
                      <span className="min-w-0 flex-1">
                        <span className={cn("block truncate text-sm text-foreground", DATAPACK_NAME_FONT)}>
                          {d.datapack_name}
                        </span>
                        {here ? (
                          // Says what will happen, since filing an already-filed
                          // datapack moves it rather than copying it.
                          <span className="block truncate text-xs text-muted-foreground">
                            moves from {here}
                          </span>
                        ) : null}
                      </span>
                    </label>
                  </li>
                );
              })
            )}
          </ul>

          {error ? (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          ) : null}
        </div>

        <DialogFooter>
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="button" disabled={picked.size === 0 || busy} onClick={() => void handleAdd()}>
            {busy ? "Adding…" : `Add ${picked.size || ""}`.trim()}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
