import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Check,
  ChevronDown,
  ChevronRight,
  Copy,
  Database,
  GitBranch,
  Plus,
  Search,
  Server,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { cn } from "@/lib/utils";
import { copyToClipboard } from "@/lib/clipboard";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import {
  createBranch,
  createCompute,
  listBranches,
  listCompute,
  listClusters,
  type AmpBranch,
  type AmpCompute,
  type AmpCluster,
} from "@/lib/api/ampApi";
import { CreateClusterDialog } from "./dialogs/CreateClusterDialog";

type Props = { datapackId: string; className?: string };

/**
 * Full-width datapack panel for AMP Postgres clusters: search the datapack's
 * clusters and expand one inline to see its branch tree and provision compute.
 * Gated by SHOW_PG_AMP at the call site.
 */
export function DatapackClustersPanel({ datapackId, className }: Props) {
  const [clusters, setClusters] = useState<AmpCluster[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setClusters(await listClusters(datapackId));
    } catch (e) {
      setError(getApiErrorMessage(e, "Failed to load clusters."));
    } finally {
      setLoading(false);
    }
  }, [datapackId]);

  useEffect(() => {
    void load();
  }, [load]);

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase();
    if (!q) return clusters;
    return clusters.filter((d) => d.name.toLowerCase().includes(q));
  }, [clusters, search]);

  return (
    <section
      className={cn(
        "w-full rounded-xl border border-border bg-card/50 shadow-sm",
        className,
      )}
    >
      <div className="flex flex-wrap items-center gap-2 border-b border-border px-4 py-3">
        <Database className="h-4 w-4 shrink-0 text-orange-400" aria-hidden />
        <span className="text-sm font-semibold text-foreground">Clusters</span>
        {!loading && !error ? (
          <span className="text-xs text-muted-foreground">{clusters.length}</span>
        ) : null}
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="ml-auto gap-2"
          onClick={() => setCreateOpen(true)}
        >
          <Plus className="h-4 w-4" aria-hidden />
          Create cluster
        </Button>
      </div>

      <div className="flex flex-col gap-3 px-4 py-4">
        <div className="relative">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Search clusters…"
            className="border border-border bg-background pl-9"
            aria-label="Search clusters"
          />
        </div>

        {loading ? (
          <p className="text-sm text-muted-foreground">Loading clusters…</p>
        ) : error ? (
          <p className="text-sm text-red-700 dark:text-red-300" role="alert">
            {error}
          </p>
        ) : clusters.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No clusters yet. Create one to spin up a Postgres cluster in this datapack.
          </p>
        ) : filtered.length === 0 ? (
          <p className="text-sm text-muted-foreground">No clusters match “{search}”.</p>
        ) : (
          <ul className="flex flex-col gap-2">
            {filtered.map((db) => (
              <ClusterRow
                key={db.id}
                datapackId={datapackId}
                db={db}
                expanded={expandedId === db.id}
                onToggle={() => setExpandedId((cur) => (cur === db.id ? null : db.id))}
              />
            ))}
          </ul>
        )}
      </div>

      <CreateClusterDialog
        open={createOpen}
        onOpenChange={setCreateOpen}
        datapackId={datapackId}
        onCreated={() => void load()}
      />
    </section>
  );
}

function ClusterRow({
  datapackId,
  db,
  expanded,
  onToggle,
}: {
  datapackId: string;
  db: AmpCluster;
  expanded: boolean;
  onToggle: () => void;
}) {
  return (
    <li className="rounded-lg border border-border bg-card">
      <button
        type="button"
        onClick={onToggle}
        className="flex w-full items-center gap-2 px-3 py-2.5 text-left"
        aria-expanded={expanded}
      >
        {expanded ? (
          <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        ) : (
          <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        )}
        <Database className="h-4 w-4 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground" title={db.name}>
          {db.name}
        </span>
        <span className="shrink-0 rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
          pg {db.default_branch.pg_version}
        </span>
      </button>
      {expanded ? (
        <div className="border-t border-border px-3 py-3">
          <ClusterBranches datapackId={datapackId} clusterId={db.id} />
        </div>
      ) : null}
    </li>
  );
}

function ClusterBranches({ datapackId, clusterId }: { datapackId: string; clusterId: string }) {
  const [branches, setBranches] = useState<AmpBranch[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [newBranchOpen, setNewBranchOpen] = useState(false);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setBranches(await listBranches(datapackId, clusterId));
    } catch (e) {
      setError(getApiErrorMessage(e, "Failed to load branches."));
    } finally {
      setLoading(false);
    }
  }, [datapackId, clusterId]);

  useEffect(() => {
    void load();
  }, [load]);

  const ordered = useMemo(() => {
    const root = branches.filter((b) => b.is_default);
    const forks = branches.filter((b) => !b.is_default);
    return [...root, ...forks];
  }, [branches]);

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2">
        <GitBranch className="h-4 w-4 shrink-0 text-orange-400" aria-hidden />
        <span className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
          Branches
        </span>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="ml-auto h-7 gap-1"
          onClick={() => setNewBranchOpen(true)}
          disabled={loading || branches.length === 0}
        >
          <Plus className="h-3.5 w-3.5" aria-hidden />
          New branch
        </Button>
      </div>

      {loading ? (
        <p className="text-xs text-muted-foreground">Loading branches…</p>
      ) : error ? (
        <p className="text-xs text-red-700 dark:text-red-300" role="alert">
          {error}
        </p>
      ) : (
        <div className="flex flex-col gap-2">
          {ordered.map((branch) => (
            <BranchCard key={branch.branch_id} datapackId={datapackId} clusterId={clusterId} branch={branch} />
          ))}
        </div>
      )}

      <NewBranchDialog
        open={newBranchOpen}
        onOpenChange={setNewBranchOpen}
        datapackId={datapackId}
        clusterId={clusterId}
        branches={branches}
        onCreated={() => void load()}
      />
    </div>
  );
}

function BranchCard({
  datapackId,
  clusterId,
  branch,
}: {
  datapackId: string;
  clusterId: string;
  branch: AmpBranch;
}) {
  return (
    <div className="rounded-md border border-border bg-background/40 p-3">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="inline-flex items-center gap-1 rounded border border-border px-1.5 py-0.5 font-mono text-xs">
          <GitBranch className="h-3 w-3" aria-hidden />
          {branch.name}
        </span>
        <span
          className={cn(
            "rounded px-1.5 py-0.5 text-[10px] font-medium",
            branch.is_default
              ? "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400"
              : "bg-teal-500/15 text-teal-600 dark:text-teal-400",
          )}
        >
          {branch.is_default ? "source" : "fork"}
        </span>
        <span className="font-mono text-xs text-muted-foreground">pg {branch.pg_version}</span>
        {branch.fork_lsn ? (
          <span className="font-mono text-xs text-muted-foreground">@ {branch.fork_lsn}</span>
        ) : null}
      </div>
      <BranchCompute datapackId={datapackId} clusterId={clusterId} branchId={branch.branch_id} />
    </div>
  );
}

function BranchCompute({
  datapackId,
  clusterId,
  branchId,
}: {
  datapackId: string;
  clusterId: string;
  branchId: string;
}) {
  const [instances, setInstances] = useState<AmpCompute[]>([]);
  const [loading, setLoading] = useState(true);
  const [role, setRole] = useState<"read_write" | "read_only">("read_write");
  const [provisioning, setProvisioning] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setInstances(await listCompute(datapackId, clusterId, branchId));
    } catch (e) {
      setError(getApiErrorMessage(e, "Failed to load compute."));
    } finally {
      setLoading(false);
    }
  }, [datapackId, clusterId, branchId]);

  useEffect(() => {
    void load();
  }, [load]);

  const provision = async () => {
    setProvisioning(true);
    setError(null);
    try {
      await createCompute(datapackId, clusterId, branchId, role);
      await load();
    } catch (e) {
      setError(getApiErrorMessage(e, "Failed to provision compute."));
    } finally {
      setProvisioning(false);
    }
  };

  return (
    <div className="mt-2 border-t border-border pt-2">
      <div className="mb-1.5 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">
        Compute
      </div>
      {loading ? (
        <p className="text-xs text-muted-foreground">Loading compute…</p>
      ) : instances.length === 0 ? (
        <p className="text-xs text-muted-foreground">No compute instances yet.</p>
      ) : (
        <ul className="flex flex-col gap-1.5">
          {instances.map((ep) => (
            <ComputeInstance key={ep.id} ep={ep} />
          ))}
        </ul>
      )}
      {error ? (
        <p className="mt-1.5 text-xs text-red-700 dark:text-red-300" role="alert">
          {error}
        </p>
      ) : null}
      <div className="mt-2 flex flex-wrap items-center gap-2">
        <select
          value={role}
          onChange={(e) => setRole(e.target.value as "read_write" | "read_only")}
          className="h-8 rounded-md border border-border bg-background px-2 text-xs text-foreground"
          aria-label="Compute role"
        >
          <option value="read_write">read-write</option>
          <option value="read_only">read-only</option>
        </select>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className="h-8"
          onClick={() => void provision()}
          disabled={provisioning}
        >
          {provisioning ? "Provisioning…" : "Provision compute"}
        </Button>
      </div>
    </div>
  );
}

function ComputeInstance({ ep }: { ep: AmpCompute }) {
  const [copied, setCopied] = useState(false);
  const psql = ep.address ? `psql "postgresql://cloud_admin@${ep.address}/postgres"` : "";

  const handleCopy = async () => {
    if (!psql) return;
    const okCopy = await copyToClipboard(psql);
    if (!okCopy) return;
    setCopied(true);
    window.setTimeout(() => setCopied(false), 2000);
  };

  return (
    <li className="rounded-md border border-border bg-card p-2">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Server className="h-3.5 w-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <span className="font-mono text-xs">{ep.address || "provisioning…"}</span>
        <span className="rounded border border-border px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
          {ep.role.replace("_", "-")}
        </span>
      </div>
      {ep.address ? (
        <div className="mt-1.5 flex items-center gap-2">
          <code className="min-w-0 flex-1 truncate rounded bg-muted px-2 py-1 font-mono text-[11px] text-muted-foreground">
            {psql}
          </code>
          <Button
            type="button"
            variant="outline"
            size="sm"
            className="h-7 shrink-0 gap-1"
            onClick={() => void handleCopy()}
            aria-label="Copy psql command"
          >
            {copied ? <Check className="h-3.5 w-3.5" aria-hidden /> : <Copy className="h-3.5 w-3.5" aria-hidden />}
          </Button>
        </div>
      ) : null}
    </li>
  );
}

function NewBranchDialog({
  open,
  onOpenChange,
  datapackId,
  clusterId,
  branches,
  onCreated,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  datapackId: string;
  clusterId: string;
  branches: AmpBranch[];
  onCreated: () => void;
}) {
  const [name, setName] = useState("");
  const [parentId, setParentId] = useState("");
  const [forkLsn, setForkLsn] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      const root = branches.find((b) => b.is_default) ?? branches[0];
      setName("");
      setParentId(root?.branch_id ?? "");
      setForkLsn("");
      setError(null);
      setSubmitting(false);
    }
  }, [open, branches]);

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!trimmed || !parentId) return;
    setSubmitting(true);
    setError(null);
    try {
      await createBranch(datapackId, clusterId, {
        name: trimmed,
        parent_branch_id: parentId,
        fork_point_lsn: forkLsn,
      });
      onCreated();
      onOpenChange(false);
    } catch (e) {
      setError(getApiErrorMessage(e, "Could not create the branch."));
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
            <DialogTitle className="text-xl text-foreground">New branch</DialogTitle>
            <DialogDescription className="text-muted-foreground">
              Forks a zero-copy clone from the parent branch. A fork point LSN is
              required when the parent has no running compute.
            </DialogDescription>
          </DialogHeader>

          <div className="my-4 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="amp-branch-parent">Fork from</Label>
              <select
                id="amp-branch-parent"
                value={parentId}
                onChange={(e) => setParentId(e.target.value)}
                className="h-9 w-full rounded-md border border-border bg-background px-2 text-sm text-foreground"
              >
                {branches.map((b) => (
                  <option key={b.branch_id} value={b.branch_id}>
                    {b.name}
                    {b.is_default ? " (default)" : ""}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="amp-branch-name">Branch name</Label>
              <Input
                id="amp-branch-name"
                placeholder="e.g. feature-x"
                value={name}
                onChange={(e) => setName(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="amp-branch-lsn">Fork point LSN (optional)</Label>
              <Input
                id="amp-branch-lsn"
                placeholder="e.g. 0/16ABCD0 — defaults to latest"
                value={forkLsn}
                onChange={(e) => setForkLsn(e.target.value)}
              />
            </div>
          </div>

          {error ? (
            <p className="text-sm text-red-700 dark:text-red-300" role="alert">
              {error}
            </p>
          ) : null}

          <DialogFooter className="gap-2 sm:gap-0">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)} disabled={submitting}>
              Cancel
            </Button>
            <Button type="submit" disabled={submitting || !name.trim() || !parentId}>
              {submitting ? "Creating…" : "Create branch"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
