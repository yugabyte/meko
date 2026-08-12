// Fleet = every cluster across the current user's shared pools.
// Stat cards + a table, and a "New cluster" dialog that creates a cluster in
// a chosen pool. Mock-only columns (topology / region / model / quota / RPO /
// status) are dropped — the controller doesn't expose them.

import { useMemo, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";
import { Database, Plus, Trash2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Combobox } from "@/components/ui/combobox";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import {
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import {
  useCurrentUser,
  useFleet,
  useStoragePools,
  type FleetRow,
} from "@/lib/api/queries";
import { formatDateTime, shortId } from "@/lib/format";
import { CreateClusterWizard } from "@/components/CreateClusterWizard";
import { DeleteClusterDialog } from "@/pages/multitenancy/components/DeleteClusterDialog";

export default function FleetPage() {
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    // ADMIN "all pools" view (temporary): show every pool, not just the current
    // user's, so meko-provisioned clusters are visible in the fleet.
    // TODO: replace with a proper admin/all-pools toggle rather than dropping
    // the owner filter outright.
    // return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
    return pools.data;
  }, [pools.data, currentUser.data]);

  // The fleet is the operator's management view: surface failed-creation orphans
  // too, so they're visible to delete (other views — counts, pickers — omit them).
  const fleet = useFleet(myPools, true);
  const [dialogOpen, setDialogOpen] = useState(false);
  // The cluster row pending deletion (null = no dialog). Delete cascades through
  // branches → endpoints, so it's type-to-confirm (see DeleteClusterDialog).
  const [toDelete, setToDelete] = useState<FleetRow | null>(null);

  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // Active pool filter comes from the `?pool=<name>` param (set when the user
  // arrives from a Shared pool tile or clicks a tab). Empty = "All".
  const activePool = searchParams.get("pool") ?? "";
  const rows = useMemo(
    () =>
      activePool
        ? fleet.rows.filter((r) => r.pool.name === activePool)
        : fleet.rows,
    [fleet.rows, activePool],
  );
  const selectTab = (name: string) =>
    setSearchParams(name ? { pool: name } : {}, { replace: true });

  const isLoading = currentUser.isLoading || pools.isLoading || fleet.isLoading;
  const error =
    currentUser.error ??
    pools.error ??
    (fleet.isError ? new Error("Failed to load cluster fleet") : null);

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Fleet management"
        title="Your cluster fleet"
        description={
          <>
            {fleet.rows.length} cluster{fleet.rows.length === 1 ? "" : "s"}{" "}
            across {myPools.length} shared pool
            {myPools.length === 1 ? "" : "s"} — every cluster provisioned inside
            your pools.
          </>
        }
        action={
          <Button
            onClick={() => setDialogOpen(true)}
            disabled={myPools.length === 0}
            variant="brand"
          >
            <Plus className="h-4 w-4" />
            New cluster
          </Button>
        }
      />

      {error ? (
        <ErrorState error={error} />
      ) : isLoading ? (
        <LoadingState label="Loading fleet…" />
      ) : (
        <>
          <div className="mb-8 grid grid-cols-1 gap-4 sm:grid-cols-2">
            <StatCard label="Clusters" value={fleet.rows.length} />
            <StatCard label="Shared pools" value={myPools.length} />
          </div>

          {/* Filter by shared pool — a searchable combobox (defaults to All),
              reflected in the URL. Type to filter; the list is height-capped and
              scrolls, so it scales to many pools without crowding the page. */}
          <div className="mb-4 flex items-center gap-2">
            <span className="text-sm text-muted-foreground">Shared pool</span>
            <Combobox
              options={[
                { value: "", label: "All shared pools" },
                ...myPools.map((p) => ({ value: p.name, label: p.name })),
              ]}
              value={activePool}
              onChange={selectTab}
              className="w-72"
              searchPlaceholder="Search pools…"
              emptyText="No pools found."
            />
          </div>

          <div className="rounded-xl border bg-card text-card-foreground shadow">
            <div className="flex items-center gap-2 border-b border-border px-5 py-4">
              <h2 className="font-semibold">Clusters</h2>
              <span className="text-sm text-muted-foreground">
                {rows.length} {activePool ? `in ${activePool}` : "total"}
              </span>
            </div>
            {rows.length === 0 ? (
              <div className="px-5 py-12 text-center text-sm text-muted-foreground">
                {activePool
                  ? `No clusters in ${activePool} yet.`
                  : "No clusters yet. Create one to get started."}
              </div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead className="text-center">Cluster</TableHead>
                    <TableHead className="text-center">Status</TableHead>
                    <TableHead className="text-center">Shared pool</TableHead>
                    <TableHead className="text-center">PG version</TableHead>
                    <TableHead className="text-center">Default branch</TableHead>
                    <TableHead className="text-center">Created</TableHead>
                    <TableHead className="text-center">Actions</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map(({ cluster, pool }) => (
                    <TableRow
                      key={cluster.id}
                      className="cursor-pointer"
                      onClick={() =>
                        navigate(
                          `/multitenancy?pool=${encodeURIComponent(pool.name)}&db=${encodeURIComponent(cluster.name)}`,
                        )
                      }
                    >
                      <TableCell>
                        <div className="flex items-center justify-center gap-3">
                          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-brand/15 text-brand">
                            <Database className="h-4 w-4" />
                          </div>
                          <div className="min-w-0 text-center">
                            <p className="truncate font-medium text-foreground">
                              {cluster.name}
                            </p>
                            <p className="font-mono text-xs text-muted-foreground">
                              {shortId(cluster.id)}
                            </p>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell className="text-center">
                        <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/15 px-2 py-0.5 text-xs font-medium text-emerald-600 dark:text-emerald-400">
                          <span className="h-1.5 w-1.5 rounded-full bg-emerald-500" />
                          Available
                        </span>
                      </TableCell>
                      <TableCell className="text-center">
                        {/* Orange = link (Meko convention). Scopes the fleet to
                            this pool; stopPropagation so it doesn't also fire the
                            row's navigate-to-branch-tree click. */}
                        <Link
                          to={`/fleet?pool=${encodeURIComponent(pool.name)}`}
                          onClick={(e) => e.stopPropagation()}
                          className="text-brand hover:underline"
                        >
                          {pool.name}
                        </Link>
                      </TableCell>
                      <TableCell className="text-center">
                        <span className="rounded-md border border-border px-2 py-0.5 font-mono text-xs">
                          pg {cluster.default_branch.pg_version}
                        </span>
                      </TableCell>
                      <TableCell className="text-center text-muted-foreground">
                        {cluster.default_branch.name}
                      </TableCell>
                      <TableCell className="text-center text-muted-foreground">
                        {formatDateTime(cluster.created_at)}
                      </TableCell>
                      <TableCell className="text-center">
                        {/* stopPropagation so deleting doesn't also fire the
                            row's navigate-to-branch-tree click. */}
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={(e) => {
                            e.stopPropagation();
                            setToDelete({ cluster, pool });
                          }}
                          className="h-7 w-7 p-0 text-destructive hover:text-destructive"
                          title={`Delete cluster ${cluster.name}`}
                          aria-label={`Delete cluster ${cluster.name}`}
                        >
                          <Trash2 className="h-4 w-4" />
                        </Button>
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </div>
        </>
      )}

      <CreateClusterWizard
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        pools={myPools}
      />

      {/* Mounted per-target so the type-to-confirm input resets each open. The
          delete cascade invalidates the cluster list, so the fleet refetches. */}
      {toDelete && (
        <DeleteClusterDialog
          open
          onOpenChange={(o) => !o && setToDelete(null)}
          poolId={toDelete.pool.id}
          clusterId={toDelete.cluster.id}
          clusterName={toDelete.cluster.name}
        />
      )}
    </PageContainer>
  );
}

function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <div className="rounded-xl border bg-card p-5 text-card-foreground shadow">
      <p className="text-xs font-medium uppercase tracking-[0.1em] text-muted-foreground">
        {label}
      </p>
      <p className="mt-2 text-3xl font-bold tracking-tight">{value}</p>
    </div>
  );
}

