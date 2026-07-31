// Shared pools = the controller's storage pools, scoped to the current user
// (owner_user_id). Card grid + a "New shared pool" dialog. Fields with no
// backing in the API (quota / tenants / topology / health from the mock) are
// intentionally omitted — only real data is shown.

import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Boxes, Database, Plus } from "lucide-react";
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
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import {
  useCreateStoragePool,
  useCurrentUser,
  useFleet,
  useStoragePools,
} from "@/lib/api/queries";
import type { StoragePool } from "@/lib/api/types";
import { formatDateTime, shortId } from "@/lib/format";

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

  // Cluster (project) counts per pool — reuse the fleet fan-out.
  const fleet = useFleet(myPools);
  const dbCountByPool = useMemo(() => {
    const counts: Record<string, number> = {};
    for (const row of fleet.rows) {
      counts[row.pool.id] = (counts[row.pool.id] ?? 0) + 1;
    }
    return counts;
  }, [fleet.rows]);

  const [dialogOpen, setDialogOpen] = useState(false);

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
              dbCount={dbCountByPool[pool.id]}
              dbCountLoading={fleet.isLoading}
              onOpen={() =>
                navigate(`/fleet?pool=${encodeURIComponent(pool.name)}`)
              }
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
    </PageContainer>
  );
}

function PoolCard({
  pool,
  dbCount,
  dbCountLoading,
  onOpen,
}: {
  pool: StoragePool;
  dbCount: number | undefined;
  dbCountLoading: boolean;
  onOpen: () => void;
}) {
  return (
    <button
      type="button"
      onClick={onOpen}
      className="flex flex-col rounded-xl border bg-card p-5 text-left text-card-foreground shadow transition-colors hover:border-brand/40"
    >
      <div className="flex items-center gap-3">
        <div className="flex h-9 w-9 shrink-0 items-center justify-center rounded-lg bg-brand/15 text-brand">
          <Boxes className="h-[18px] w-[18px]" />
        </div>
        <div className="min-w-0">
          <p className="truncate font-semibold">{pool.name}</p>
          <p className="font-mono text-xs text-muted-foreground">
            {shortId(pool.id)}
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
