// Multitenancy = the data-branching workflow. Pick a shared pool, then a
// cluster in it, then view that cluster's branch tree and fork
// sub-branches off any branch. (Tenant/user management is bootstrapped
// elsewhere — see useCurrentUser — so it isn't surfaced here.)

import { useMemo, useState } from "react";
import { engineLabel } from "@/components/ui/engine-field";
import { useSearchParams } from "react-router-dom";
import { GitBranch, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import {
  useBranches,
  useCurrentUser,
  useClusters,
  useStoragePools,
} from "@/lib/api/queries";
import { dataBranchingMessages as msg } from "@/lib/messages";
import { BranchTree } from "./multitenancy/components/BranchTree";
import { CreateBranchDialog } from "./multitenancy/components/CreateBranchDialog";

export default function MultitenancyPage() {
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  // ADMIN "all pools" view (temporary): show every pool, not just the current
  // user's, so the Shared pool dropdown matches Shared pools / Fleet.
  // TODO: replace with a proper admin/all-pools toggle rather than dropping the
  // owner filter outright.
  // return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    return pools.data;
  }, [pools.data, currentUser.data]);

  // Selection lives in the URL (?pool=<name>&db=<name>) so it's shareable and
  // deep-linkable from the Fleet table. Names resolve to ids for the API calls.
  const [searchParams, setSearchParams] = useSearchParams();
  const poolName = searchParams.get("pool") ?? "";
  const dbName = searchParams.get("db") ?? "";

  const poolId = useMemo(
    () => myPools.find((p) => p.name === poolName)?.id ?? "",
    [myPools, poolName],
  );
  const clusters = useClusters(poolId || undefined);
  const clusterId = useMemo(
    () => clusters.data?.find((p) => p.name === dbName)?.id ?? "",
    [clusters.data, dbName],
  );
  const cluster = clusters.data?.find((p) => p.name === dbName);
  const branches = useBranches(poolId || undefined, clusterId || undefined);
  const [newBranchOpen, setNewBranchOpen] = useState(false);

  const selectPool = (name: string) =>
    setSearchParams(name ? { pool: name } : {}, { replace: true });
  const selectDb = (name: string) =>
    setSearchParams(name ? { pool: poolName, db: name } : { pool: poolName }, {
      replace: true,
    });

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Cluster"
        title="Endpoints and Branches"
        description="Pick a shared pool and one of its clusters to see its branch tree and the endpoints serving each branch, then fork zero-copy sub-branches off any branch."
        action={
          <Button
            onClick={() => setNewBranchOpen(true)}
            disabled={!clusterId || !branches.data?.length}
            variant="brand"
          >
            <Plus className="h-4 w-4" />
            New branch
          </Button>
        }
      />

      {pools.error || currentUser.error ? (
        <ErrorState error={pools.error ?? currentUser.error} />
      ) : (
        <>
          <div className="mb-5 grid grid-cols-1 gap-3 sm:grid-cols-2">
            <div className="space-y-2">
              <Label>Shared pool</Label>
              <Select value={poolName} onValueChange={selectPool}>
                <SelectTrigger>
                  <SelectValue
                    placeholder={
                      pools.isLoading || currentUser.isLoading
                        ? "Loading pools…"
                        : myPools.length
                          ? "Select a pool"
                          : "No pools yet"
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {myPools.map((pool) => (
                    <SelectItem key={pool.id} value={pool.name}>
                      {pool.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label>Cluster</Label>
              <Select
                value={dbName}
                onValueChange={selectDb}
                disabled={!poolId || !clusters.data?.length}
              >
                <SelectTrigger>
                  <SelectValue
                    placeholder={
                      !poolId
                        ? "Pick a pool first"
                        : clusters.error
                          ? "Error loading clusters"
                          : clusters.isLoading
                            ? "Loading clusters…"
                            : clusters.data?.length
                              ? "Select a cluster"
                              : "No clusters in this pool"
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {clusters.data?.map((cluster) => (
                    <SelectItem key={cluster.id} value={cluster.name}>
                      {cluster.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>

          <div className="flex items-center gap-2 pb-3 text-sm text-muted-foreground">
            <GitBranch className="h-4 w-4 text-brand" />
            Zero-copy thin clones · copy-on-write · fork at an LSN
          </div>

          {clusterId && branches.data?.length ? (
            <div className="mb-4 grid grid-cols-2 gap-px overflow-hidden rounded-xl border bg-border sm:grid-cols-4">
              {[
                { k: "Cluster", v: dbName },
                {
                  k: "Engine",
                  v: engineLabel(cluster?.engine, branches.data[0]?.pg_version),
                },
                { k: "Cluster ID", v: clusterId, mono: true },
                { k: "Branches", v: String(branches.data.length) },
              ].map(({ k, v, mono }) => (
                <div key={k} className="bg-card p-3">
                  <div className="text-[11px] uppercase tracking-wide text-muted-foreground">{k}</div>
                  {/* A uuid is 36 chars: sized to fit the tile on one line at
                      the 4-column breakpoint, and wrapping rather than
                      truncating below it. */}
                  <div
                    className={
                      mono
                        ? "mt-1 break-all font-mono text-[11px] font-medium leading-5 text-foreground"
                        : "mt-1 truncate text-sm font-medium text-foreground"
                    }
                  >
                    {v}
                  </div>
                </div>
              ))}
            </div>
          ) : null}

          <div className="rounded-xl border bg-card p-5 text-card-foreground shadow">
            {!clusterId ? (
              <p className="py-8 text-center text-sm text-muted-foreground">
                {msg.selectPoolAndCluster}
              </p>
            ) : branches.error ? (
              <ErrorState error={branches.error} />
            ) : branches.isLoading ? (
              <LoadingState label="Loading branches…" />
            ) : (
              <BranchTree
                poolId={poolId}
                clusterId={clusterId}
                branches={branches.data ?? []}
              />
            )}
          </div>
        </>
      )}

      <CreateBranchDialog
        open={newBranchOpen}
        onOpenChange={setNewBranchOpen}
        poolId={poolId}
        clusterId={clusterId}
        branches={branches.data ?? []}
      />
    </PageContainer>
  );
}
