// Full-width datapack panel for AMP Postgres clusters: search the datapack's
// clusters and expand one inline to see its branch tree and provision compute.
// Gated by SHOW_PG_AMP at the call site. The cluster/branch/compute pieces live
// as modular components under ./datapack-clusters/.

import { useMemo, useState } from "react";
import { Database, Plus, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { useClusters } from "@/features/datapacks/hooks/useAmpClusters";
import { CreateClusterDialog } from "./dialogs/CreateClusterDialog";
import { ClusterRow } from "./datapack-clusters/ClusterRow";

type Props = { datapackId: string; className?: string };

export function DatapackClustersPanel({ datapackId, className }: Props) {
  const clustersQuery = useClusters(datapackId);
  const clusters = clustersQuery.data ?? [];
  const loading = clustersQuery.isLoading;
  const error = clustersQuery.error ? getApiErrorMessage(clustersQuery.error, "Failed to load clusters.") : null;
  const [search, setSearch] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [createOpen, setCreateOpen] = useState(false);

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
      />
    </section>
  );
}
