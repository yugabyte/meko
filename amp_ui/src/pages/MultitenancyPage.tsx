// Multitenancy = the data-branching workflow. Pick a shared pool, then a
// database (project) in it, then view that project's branch tree and fork
// sub-branches off any branch. (Tenant/user management is bootstrapped
// elsewhere — see useCurrentUser — so it isn't surfaced here.)

import { useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { Check, Copy, GitBranch, Loader2, Plus, Server } from "lucide-react";
import { toast } from "sonner";
import { cn } from "@/lib/utils";
import { Badge } from "@/components/ui/badge";
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
  ErrorState,
  LoadingState,
  PageContainer,
  PageHeader,
} from "@/components/data/Page";
import {
  useBranches,
  useCreateBranch,
  useCurrentUser,
  useProjects,
  useStoragePools,
} from "@/lib/api/queries";
import { useBranchEndpoints, useCreateEndpoint } from "@/hooks/useEndpoints";
import type {
  Branch,
  CreateBranchInput,
  Endpoint,
  EndpointRole,
} from "@/lib/api/types";
import { formatDateTime, shortId } from "@/lib/format";

export default function MultitenancyPage() {
  const currentUser = useCurrentUser();
  const pools = useStoragePools();

  // Only the current user's pools (owner_user_id) — matches Shared pools / Fleet.
  const myPools = useMemo(() => {
    if (!pools.data || !currentUser.data) return [];
    return pools.data.filter((p) => p.owner_user_id === currentUser.data.id);
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
  const projects = useProjects(poolId || undefined);
  const projectId = useMemo(
    () => projects.data?.find((p) => p.name === dbName)?.id ?? "",
    [projects.data, dbName],
  );
  const branches = useBranches(poolId || undefined, projectId || undefined);
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
        eyebrow="Multitenancy"
        title="Data branching"
        description="Pick a shared pool and one of its databases to see the branch tree, then fork zero-copy sub-branches off any branch."
        action={
          <Button
            onClick={() => setNewBranchOpen(true)}
            disabled={!projectId || !branches.data?.length}
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
              <Label>Database</Label>
              <Select
                value={dbName}
                onValueChange={selectDb}
                disabled={!poolId || !projects.data?.length}
              >
                <SelectTrigger>
                  <SelectValue
                    placeholder={
                      !poolId
                        ? "Pick a pool first"
                        : projects.error
                          ? "Error loading databases"
                          : projects.isLoading
                            ? "Loading databases…"
                            : projects.data?.length
                              ? "Select a database"
                              : "No databases in this pool"
                    }
                  />
                </SelectTrigger>
                <SelectContent>
                  {projects.data?.map((project) => (
                    <SelectItem key={project.id} value={project.name}>
                      {project.name}
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

          <div className="rounded-xl border bg-card p-5 text-card-foreground shadow">
            {!projectId ? (
              <p className="py-8 text-center text-sm text-muted-foreground">
                Select a pool and database to view its branch tree.
              </p>
            ) : branches.error ? (
              <ErrorState error={branches.error} />
            ) : branches.isLoading ? (
              <LoadingState label="Loading branches…" />
            ) : (
              <BranchTree
                poolId={poolId}
                projectId={projectId}
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
        projectId={projectId}
        branches={branches.data ?? []}
      />
    </PageContainer>
  );
}

function BranchTree({
  poolId,
  projectId,
  branches,
}: {
  poolId: string;
  projectId: string;
  branches: Branch[];
}) {
  // Default branch is the source/root; everything else is a fork shown below.
  const root = branches.find((b) => b.is_default);
  const forks = branches.filter((b) => !b.is_default);
  const nameById = useMemo(() => {
    const map: Record<string, string> = {};
    for (const b of branches) map[b.branch_id] = b.name;
    return map;
  }, [branches]);

  const endpoints = useBranchEndpoints(poolId, projectId, branches);

  if (branches.length === 0) {
    return (
      <p className="py-6 text-center text-sm text-muted-foreground">
        No branches found for this database.
      </p>
    );
  }

  const ordered = root ? [root, ...forks] : forks;
  return (
    <div className="space-y-3">
      {ordered.map((branch) => (
        <BranchCard
          key={branch.branch_id}
          branch={branch}
          isRoot={branch.is_default}
          parentName={
            branch.parent_branch_id
              ? nameById[branch.parent_branch_id]
              : undefined
          }
          poolId={poolId}
          projectId={projectId}
          endpoints={endpoints.byBranch[branch.branch_id] ?? []}
          endpointsLoading={endpoints.isLoading}
        />
      ))}
    </div>
  );
}

function BranchCard({
  branch,
  isRoot,
  parentName,
  poolId,
  projectId,
  endpoints,
  endpointsLoading,
}: {
  branch: Branch;
  isRoot: boolean;
  parentName?: string;
  poolId: string;
  projectId: string;
  endpoints: Endpoint[];
  endpointsLoading: boolean;
}) {
  return (
    <div
      className={cn(
        "rounded-lg border border-border bg-background/40 p-4",
        !isRoot && "ml-6",
      )}
    >
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge variant="outline" className="gap-1 font-mono">
          <GitBranch className="h-3 w-3" />
          {branch.name}
        </Badge>
        {isRoot ? (
          <Badge className="bg-emerald-500/15 text-emerald-600 dark:text-emerald-400">
            source
          </Badge>
        ) : (
          <Badge className="bg-teal-500/15 text-teal-600 dark:text-teal-400">
            fork
          </Badge>
        )}
        <span className="font-mono text-xs text-muted-foreground">
          pg {branch.pg_version}
        </span>
        <span className="font-mono text-xs text-muted-foreground">
          {shortId(branch.branch_id)}
        </span>
        {parentName && (
          <span className="text-xs text-muted-foreground">
            forked from{" "}
            <span className="font-medium text-foreground">{parentName}</span>
            {branch.fork_lsn && (
              <>
                {" "}
                @ <span className="font-mono">{branch.fork_lsn}</span>
              </>
            )}
          </span>
        )}
        <span className="ml-auto text-xs text-muted-foreground">
          {formatDateTime(branch.created_at)}
        </span>
      </div>

      <EndpointsSection
        poolId={poolId}
        projectId={projectId}
        branchId={branch.branch_id}
        endpoints={endpoints}
        loading={endpointsLoading}
      />
    </div>
  );
}

function EndpointsSection({
  poolId,
  projectId,
  branchId,
  endpoints,
  loading,
}: {
  poolId: string;
  projectId: string;
  branchId: string;
  endpoints: Endpoint[];
  loading: boolean;
}) {
  const [role, setRole] = useState<EndpointRole>("read_write");
  const createEndpoint = useCreateEndpoint();

  function provision() {
    createEndpoint.mutate(
      { poolId, projectId, branchId, role },
      {
        onSuccess: (ep) =>
          toast.success(`Compute provisioned (${ep.role.replace("_", "-")})`),
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : "Failed to provision compute",
          ),
      },
    );
  }

  return (
    <div className="mt-3 border-t border-border pt-3">
      <div className="mb-2 text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        Compute
      </div>
      {loading ? (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <Loader2 className="h-3 w-3 animate-spin" />
          Loading compute…
        </p>
      ) : endpoints.length ? (
        <ul className="space-y-2">
          {endpoints.map((ep) => (
            <ComputeInstance key={ep.id} ep={ep} />
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">No compute instances yet.</p>
      )}

      <div className="mt-3 flex flex-wrap items-center gap-2">
        <Select value={role} onValueChange={(v) => setRole(v as EndpointRole)}>
          <SelectTrigger className="h-8 w-36">
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="read_write">read-write</SelectItem>
            <SelectItem value="read_only">read-only</SelectItem>
          </SelectContent>
        </Select>
        <Button
          variant="outline"
          size="sm"
          onClick={provision}
          disabled={createEndpoint.isPending}
        >
          {createEndpoint.isPending ? "Provisioning…" : "Provision compute"}
        </Button>
      </div>
    </div>
  );
}

// One provisioned compute instance: its address, role, and a copyable psql
// connect string. The controller returns `address` (host:port) once the
// instance is deployed; it connects as user `cloud_admin`, database `postgres`.
function ComputeInstance({ ep }: { ep: Endpoint }) {
  const [copied, setCopied] = useState(false);
  const connStr = ep.address
    ? `postgresql://cloud_admin@${ep.address}/postgres`
    : "";
  const psql = connStr ? `psql "${connStr}"` : "";

  function copy() {
    if (!psql || !navigator.clipboard) return;
    navigator.clipboard
      .writeText(psql)
      .then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), 1500);
      })
      .catch((err) => {
        toast.error("Couldn't copy to clipboard");
        console.error("Clipboard copy failed:", err);
      });
  }

  return (
    <li className="rounded-md border border-border bg-background/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Server className="h-4 w-4 text-muted-foreground" />
        <span className="font-mono text-xs">
          {ep.address || "provisioning…"}
        </span>
        <Badge variant="secondary" className="font-mono text-[10px]">
          {ep.role.replace("_", "-")}
        </Badge>
      </div>
      {ep.address && (
        <div className="mt-2 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-xs text-muted-foreground">
            {psql}
          </code>
          <Button
            variant="outline"
            size="sm"
            onClick={copy}
            className="h-7 shrink-0 gap-1"
          >
            {copied ? (
              <Check className="h-3.5 w-3.5" />
            ) : (
              <Copy className="h-3.5 w-3.5" />
            )}
            {copied ? "Copied" : "Copy"}
          </Button>
        </div>
      )}
    </li>
  );
}

function CreateBranchDialog({
  open,
  onOpenChange,
  poolId,
  projectId,
  branches,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  poolId: string;
  projectId: string;
  branches: Branch[];
}) {
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState<string>("");
  const [forkLsn, setForkLsn] = useState("");
  const createBranch = useCreateBranch();

  // Default the parent to the source branch when the dialog opens.
  useEffect(() => {
    if (open) {
      const root = branches.find((b) => b.is_default) ?? branches[0];
      setParentId(root?.branch_id ?? "");
      setForkLsn("");
    }
  }, [open, branches]);

  const parent = branches.find((b) => b.branch_id === parentId);

  function submit(e: React.FormEvent) {
    e.preventDefault();
    const trimmed = name.trim();
    if (!trimmed || !parent) return;
    // Controller's CreateBranchRequest only accepts name / parent_branch_id /
    // fork_point_lsn. Send the LSN only when given; otherwise the controller
    // reads the parent's latest LSN — which 400s if the parent has no running
    // endpoint, so the field is offered for that case.
    const body: CreateBranchInput = {
      name: trimmed,
      parent_branch_id: parent.branch_id,
    };
    const lsn = forkLsn.trim();
    if (lsn) body.fork_point_lsn = lsn;

    createBranch.mutate(
      { poolId, projectId, body },
      {
        onSuccess: (branch) => {
          toast.success(`Branch "${branch.name}" created`);
          setName("");
          setForkLsn("");
          onOpenChange(false);
        },
        onError: (err) =>
          toast.error(
            err instanceof Error ? err.message : "Failed to create branch",
          ),
      },
    );
  }

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent>
        <form onSubmit={submit}>
          <DialogHeader>
            <DialogTitle>New sub-branch</DialogTitle>
            <DialogDescription>
              Forks a zero-copy clone from the parent branch at its current LSN.
            </DialogDescription>
          </DialogHeader>
          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="branch-parent">Fork from</Label>
              <Select value={parentId} onValueChange={setParentId}>
                <SelectTrigger id="branch-parent">
                  <SelectValue placeholder="Select a parent branch" />
                </SelectTrigger>
                <SelectContent>
                  {branches.map((b) => (
                    <SelectItem key={b.branch_id} value={b.branch_id}>
                      {b.name}
                      {b.is_default ? " (default)" : ""}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="branch-name">Branch name</Label>
              <Input
                id="branch-name"
                placeholder="e.g. agent_eval_a"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="branch-lsn">Fork point LSN (optional)</Label>
              <Input
                id="branch-lsn"
                placeholder="e.g. 0/16ABCD0 — defaults to latest"
                value={forkLsn}
                onChange={(e) => setForkLsn(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                Required when the parent branch has no running endpoint to read
                its latest LSN from.
              </p>
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
              disabled={!name.trim() || !parent || createBranch.isPending}
              variant="brand"
            >
              {createBranch.isPending ? "Creating…" : "Create branch"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
