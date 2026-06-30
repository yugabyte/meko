// Fleet = every database (project) across the current user's shared pools.
// Stat cards + a table, and a "New database" dialog that creates a project in
// a chosen pool. Mock-only columns (topology / region / model / quota / RPO /
// status) are dropped — the controller doesn't expose them.

import { useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Database, Plus } from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";
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
  useCreateProject,
  useCurrentUser,
  useFleet,
  useStoragePools,
} from "@/lib/api/queries";
import type { StoragePool } from "@/lib/api/types";
import { formatDateTime, shortId } from "@/lib/format";

export default function FleetPage() {
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
  }, [pools.data, currentUser.data]);

  const fleet = useFleet(myPools);
  const [dialogOpen, setDialogOpen] = useState(false);

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
    (fleet.isError ? new Error("Failed to load database fleet") : null);

  return (
    <PageContainer>
      <PageHeader
        eyebrow="Fleet management"
        title="Your database fleet"
        description={
          <>
            {fleet.rows.length} database{fleet.rows.length === 1 ? "" : "s"}{" "}
            across {myPools.length} shared pool
            {myPools.length === 1 ? "" : "s"} — every project provisioned inside
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
            New database
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
            <StatCard label="Databases" value={fleet.rows.length} />
            <StatCard label="Shared pools" value={myPools.length} />
          </div>

          {/* Filter tabs — "All" + one per shared pool, reflected in the URL. */}
          <div className="mb-4 flex flex-wrap gap-2">
            <FleetTab active={!activePool} onClick={() => selectTab("")}>
              All
            </FleetTab>
            {myPools.map((p) => (
              <FleetTab
                key={p.id}
                active={activePool === p.name}
                onClick={() => selectTab(p.name)}
              >
                {p.name}
              </FleetTab>
            ))}
          </div>

          <div className="rounded-xl border bg-card text-card-foreground shadow">
            <div className="flex items-center gap-2 border-b border-border px-5 py-4">
              <h2 className="font-semibold">Databases</h2>
              <span className="text-sm text-muted-foreground">
                {rows.length} {activePool ? `in ${activePool}` : "total"}
              </span>
            </div>
            {rows.length === 0 ? (
              <div className="px-5 py-12 text-center text-sm text-muted-foreground">
                {activePool
                  ? `No databases in ${activePool} yet.`
                  : "No databases yet. Create one to get started."}
              </div>
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Database</TableHead>
                    <TableHead>Shared pool</TableHead>
                    <TableHead>PG version</TableHead>
                    <TableHead>Default branch</TableHead>
                    <TableHead className="text-right">Created</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {rows.map(({ project, pool }) => (
                    <TableRow
                      key={project.id}
                      className="cursor-pointer"
                      onClick={() =>
                        navigate(
                          `/multitenancy?pool=${encodeURIComponent(pool.name)}&db=${encodeURIComponent(project.name)}`,
                        )
                      }
                    >
                      <TableCell>
                        <div className="flex items-center gap-3">
                          <div className="flex h-8 w-8 shrink-0 items-center justify-center rounded-md bg-brand/15 text-brand">
                            <Database className="h-4 w-4" />
                          </div>
                          <div className="min-w-0">
                            <p className="truncate font-medium text-foreground">
                              {project.name}
                            </p>
                            <p className="font-mono text-xs text-muted-foreground">
                              {shortId(project.id)}
                            </p>
                          </div>
                        </div>
                      </TableCell>
                      <TableCell>
                        <span className="text-teal-600 dark:text-teal-400">
                          {pool.name}
                        </span>
                      </TableCell>
                      <TableCell>
                        <span className="rounded-md border border-border px-2 py-0.5 font-mono text-xs">
                          pg {project.default_branch.pg_version}
                        </span>
                      </TableCell>
                      <TableCell className="text-muted-foreground">
                        {project.default_branch.name}
                      </TableCell>
                      <TableCell className="text-right text-muted-foreground">
                        {formatDateTime(project.created_at)}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </div>
        </>
      )}

      <CreateDatabaseDialog
        open={dialogOpen}
        onOpenChange={setDialogOpen}
        pools={myPools}
      />
    </PageContainer>
  );
}

function FleetTab({
  active,
  onClick,
  children,
}: {
  active: boolean;
  onClick: () => void;
  children: React.ReactNode;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "rounded-md border px-3 py-1.5 text-sm font-medium transition-colors",
        active
          ? "border-brand/40 bg-brand/10 text-brand"
          : "border-border text-muted-foreground hover:border-brand/30 hover:text-foreground",
      )}
    >
      {children}
    </button>
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

function CreateDatabaseDialog({
  open,
  onOpenChange,
  pools,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pools: StoragePool[];
}) {
  const [name, setName] = useState("");
  const [poolId, setPoolId] = useState<string>("");
  const createProject = useCreateProject();

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed || !poolId) return;
    createProject.mutate(
      { poolId, name: trimmed },
      {
        onSuccess: (project) => {
          toast.success(`Database "${project.name}" created`);
          setName("");
          setPoolId("");
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : "Failed to create database",
          ),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>New database</DialogTitle>
            <DialogDescription>
              Creates a project (database) inside a shared pool, with a default{" "}
              <code>main</code> branch.
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="db-pool">Shared pool</Label>
              <Select value={poolId} onValueChange={setPoolId}>
                <SelectTrigger id="db-pool">
                  <SelectValue placeholder="Select a pool" />
                </SelectTrigger>
                <SelectContent>
                  {pools.map((pool) => (
                    <SelectItem key={pool.id} value={pool.id}>
                      {pool.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="db-name">Database name</Label>
              <Input
                id="db-name"
                placeholder="e.g. orders"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
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
              disabled={!name.trim() || !poolId || createProject.isPending}
              variant="brand"
            >
              {createProject.isPending ? "Creating…" : "Create database"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
