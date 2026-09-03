// Shared pools = the controller's storage pools, scoped to the current user
// (owner_user_id). Card grid + a "New shared pool" dialog. Fields with no
// backing in the API (quota / tenants / topology / health from the mock) are
// intentionally omitted — only real data is shown.

import { useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Boxes, Database, MoreVertical, Pencil, Plus, Trash2 } from "lucide-react";
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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import {
  useCreateStoragePool,
  useCurrentUser,
  useDeleteStoragePool,
  useFleet,
  useRenameStoragePool,
  useStoragePools,
} from "@/lib/api/queries";
import type { StoragePool } from "@/lib/api/types";
import { formatDateTime } from "@/lib/format";

export default function SharedPoolsPage() {
  const navigate = useNavigate();
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    // ADMIN "all pools" view (temporary): show every pool, not just the current
    // user's, so meko-provisioned pools (owned by the meko user) are visible.
    // TODO: replace with a proper admin/all-pools toggle rather than dropping
    // the owner filter outright.
    // return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
    return pools.data;
  }, [pools.data, currentUser.data]);

  // Cluster counts per pool — reuse the fleet fan-out. includeFailed, because
  // this count also gates Delete: a failed-creation cluster is still a live row
  // and the controller counts it, so hiding those would offer a delete that 409s.
  const fleet = useFleet(myPools, true);
  const dbCountByPool = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const row of fleet.rows) {
      counts[row.pool.id] = (counts[row.pool.id] ?? 0) + 1;
    }
    return counts;
  }, [fleet.rows]);

  const [dialogOpen, setDialogOpen] = useState(false);
  const [renaming, setRenaming] = useState<StoragePool | null>(null);
  const [deleting, setDeleting] = useState<StoragePool | null>(null);

  const isLoading = currentUser.isLoading || pools.isLoading;
  const error = currentUser.error ?? pools.error;

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Multitenancy"
        eyebrowBadge="Tenant admin"
        title="Shared pools"
        description="A shared pool is a team or business unit sitting on a foundation. Clusters are created inside a pool, which owns its clusters and branches."
        action={
          <Button
            onClick={() => setDialogOpen(true)}
            disabled={!currentUser.data}
            variant="brand"
          >
            <Plus className="h-4 w-4" />
            New shared pool
          </Button>
        }
      />

      {error ? (
        <ErrorState error={error} />
      ) : isLoading ? (
        <LoadingState label="Loading shared pools…" />
      ) : (
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {myPools.map((pool) => (
            <PoolCard
              key={pool.id}
              pool={pool}
              // A pool with no clusters contributes no fleet rows, so it has no
              // entry at all — resolve that to 0 once the fan-out has settled,
              // or an empty pool would read as "count unknown" forever.
              dbCount={fleet.isLoading ? undefined : (dbCountByPool[pool.id] ?? 0)}
              dbCountLoading={fleet.isLoading}
              onOpen={() =>
                navigate(`/fleet?pool=${encodeURIComponent(pool.name)}`)
              }
              onRename={() => setRenaming(pool)}
              onDelete={() => setDeleting(pool)}
            />
          ))}
          <button
            type="button"
            onClick={() => setDialogOpen(true)}
            disabled={!currentUser.data}
            className="flex min-h-[160px] flex-col items-center justify-center gap-2 rounded-xl border border-dashed border-border text-muted-foreground transition-colors hover:border-brand/60 hover:text-foreground disabled:cursor-not-allowed disabled:opacity-50"
          >
            <Plus className="h-5 w-5" />
            <span className="text-sm font-medium">New shared pool</span>
            <span className="text-xs">Team or business unit</span>
          </button>
        </div>
      )}

      <CreatePoolDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        ownerUserId={currentUser.data?.id}
      />
      <RenamePoolDialog
        pool={renaming}
        onOpenChange={(open) => !open && setRenaming(null)}
      />
      <DeletePoolDialog
        pool={deleting}
        onOpenChange={(open) => !open && setDeleting(null)}
      />
    </PageContainer>
  );
}

export function PoolCard({
  pool,
  dbCount,
  dbCountLoading,
  onOpen,
  onRename,
  onDelete,
}: {
  pool: StoragePool;
  dbCount: number | undefined;
  dbCountLoading: boolean;
  onOpen: () => void;
  onRename: () => void;
  onDelete: () => void;
}) {
  // The controller refuses to delete a pool that still holds a live cluster, so
  // disable the action rather than let it 409. An unresolved count is treated as
  // non-empty: better to withhold the action than offer one that fails.
  const hasClusters = dbCount === undefined || dbCount > 0;
  return (
    <div className="relative flex flex-col rounded-xl border bg-card text-card-foreground shadow transition-colors hover:border-brand/40">
      <div className="absolute right-2 top-2">
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              aria-label={`Actions for ${pool.name}`}
            >
              <MoreVertical className="h-4 w-4" />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <DropdownMenuItem onSelect={onRename}>
              <Pencil className="mr-2 h-4 w-4" />
              Rename
            </DropdownMenuItem>
            <DropdownMenuItem
              className="text-destructive focus:text-destructive"
              disabled={hasClusters}
              onSelect={onDelete}
            >
              <Trash2 className="mr-2 h-4 w-4" />
              Delete
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>

      <button
        type="button"
        onClick={onOpen}
        className="flex flex-1 flex-col p-5 text-left"
      >
        <div className="flex items-center gap-3 pr-8">
          <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-brand/15 text-brand">
            <Boxes className="h-[18px] w-[18px]" />
          </div>
          <div className="min-w-0">
            <p className="truncate font-semibold">{pool.name}</p>
            <p className="font-mono text-xs text-muted-foreground">
              {pool.id}
            </p>
          </div>
        </div>

        <div className="mt-5 flex items-center justify-between border-t border-border pt-4 text-sm">
          <span className="flex items-center gap-2 text-muted-foreground">
            <Database className="h-4 w-4" />
            Clusters
          </span>
          <span className="font-semibold text-foreground">
            {dbCountLoading && dbCount === undefined ? "…" : (dbCount ?? 0)}
          </span>
        </div>
        <p className="mt-3 text-xs text-muted-foreground">
          Created {formatDateTime(pool.created_at)}
        </p>
      </button>
    </div>
  );
}

export function RenamePoolDialog({
  pool,
  onOpenChange,
}: {
  pool: StoragePool | null;
  onOpenChange: (open: boolean) => void;
}) {
  const [name, setName] = useState("");
  const rename = useRenameStoragePool();

  // Seed the field from whichever pool the menu opened on.
  useEffect(() => {
    if (pool) setName(pool.name);
  }, [pool]);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!pool || !trimmed || trimmed === pool.name) return;
    rename.mutate(
      { poolId: pool.id, name: trimmed },
      {
        onSuccess: (updated) => {
          toast.success(`Shared pool renamed to "${updated.name}"`);
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : "Failed to rename pool"),
      },
    );
  }

  return (
    <Dialog open={!!pool} onOpenChange={(o) => !rename.isPending && onOpenChange(o)}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>Rename shared pool</DialogTitle>
            <DialogDescription>
              Nothing addresses a pool by name, so renaming one breaks no
              reference. The name must be free.
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-2">
            <Label htmlFor="rename-pool-name">Pool name</Label>
            <Input
              id="rename-pool-name"
              autoFocus
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => onOpenChange(false)}
              disabled={rename.isPending}
            >
              Cancel
            </Button>
            <Button
              type="submit"
              disabled={
                !name.trim() || name.trim() === pool?.name || rename.isPending
              }
              variant="brand"
            >
              {rename.isPending ? "Renaming…" : "Rename pool"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

// A one-click confirm, not the type-to-confirm DeleteClusterDialog uses: the
// pool is empty by the time this is reachable, so nothing is destroyed.
function DeletePoolDialog({
  pool,
  onOpenChange,
}: {
  pool: StoragePool | null;
  onOpenChange: (open: boolean) => void;
}) {
  const del = useDeleteStoragePool();

  function remove() {
    if (!pool) return;
    del.mutate(
      { poolId: pool.id },
      {
        onSuccess: () => {
          toast.success(`Shared pool "${pool.name}" deleted`);
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : "Failed to delete pool"),
      },
    );
  }

  return (
    <Dialog open={!!pool} onOpenChange={(o) => !del.isPending && onOpenChange(o)}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Delete shared pool &ldquo;{pool?.name}&rdquo;?</DialogTitle>
          <DialogDescription>
            The pool holds no clusters, so nothing is lost. Its name becomes
            available again straight away.
          </DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button
            type="button"
            variant="outline"
            onClick={() => onOpenChange(false)}
            disabled={del.isPending}
          >
            Cancel
          </Button>
          <Button
            type="button"
            variant="destructive"
            onClick={remove}
            disabled={del.isPending}
          >
            {del.isPending ? "Deleting…" : "Delete pool"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

function CreatePoolDialog({
  open,
  onOpenChange,
  ownerUserId,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  ownerUserId: string | undefined;
}) {
  const [name, setName] = useState("");
  const createPool = useCreateStoragePool();

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed || !ownerUserId) return;
    createPool.mutate(
      { name: trimmed, ownerUserId },
      {
        onSuccess: (pool) => {
          toast.success(`Shared pool "${pool.name}" created`);
          setName("");
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(err instanceof Error ? err.message : "Failed to create pool"),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>New shared pool</DialogTitle>
            <DialogDescription>
              A pool groups clusters for a team or business unit. It&apos;s
              owned by the current tenant.
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-2">
            <Label htmlFor="pool-name">Pool name</Label>
            <Input
              id="pool-name"
              autoFocus
              placeholder="e.g. payments-bu"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
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
              disabled={!name.trim() || !ownerUserId || createPool.isPending}
              variant="brand"
            >
              {createPool.isPending ? "Creating…" : "Create pool"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
