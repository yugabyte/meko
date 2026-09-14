// Provisions (or picks) exactly ONE real AMP cluster for this datapack --
// unlike the full datapack-detail AMP UI (features/datapacks/.../CreateClusterWizard.tsx),
// this lab has no need for engine/extension/compute-sizing choices, so it
// calls useCreateCluster directly with just a name (plus the same DEFAULT_CPU
// budget CreateClusterWizard prefills, so a free-tier account doesn't 402 --
// see the comment on handleCreate) instead of embedding that multi-step
// wizard. `with_writer: true` (baked into ampCreateCluster) means one create
// call provisions the cluster, its `main` branch, AND a read-write compute
// in a single background task -- the returned cluster already has
// everything session.selectAmpCluster needs.
//
// Real, working provisioning (see useAmpClusters.ts/ampApi.ts) -- what's NOT
// wired up yet is writing review-run rows into it; see review-dashboard's
// own comment for that gap.

import { useState } from "react";
import { AlertCircle, Database, Loader2, Plus } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ampCoreLimitMessage } from "@/lib/api/ampApi";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { isAmpEnabled } from "@/lib/amp";
import { useAmpCoreBudget, useCreateCluster } from "@/features/datapacks/hooks/useAmpClusters";
import { DEFAULT_CPU } from "@/features/datapacks/components/datapack-detail/datapack-clusters/resize";
import { CoreLimitNotice } from "@/features/datapacks/components/datapack-detail/datapack-clusters/CoreLimitNotice";

import { LabStageNeedsDatapack } from "../lab-runner/LabStageNeedsDatapack";
import { useValidAmpCluster } from "../../hooks/useValidAmpCluster";
import type { useLabRunnerSession } from "../../hooks/useLabRunnerSession";
import { LabStageHeader } from "./LabStageHeader";

// "standard" -> "Standard"; empty next_tier -> a safe generic. Mirrors
// DatapackClustersPanel.tsx's own tierLabel (not exported/shared -- a
// 2-line pure function, cheaper to duplicate than to add a cross-feature
// import for).
function tierLabel(tier: string | undefined): string {
  if (!tier) return "a higher plan";
  return tier.charAt(0).toUpperCase() + tier.slice(1);
}

export function CreateAmpClusterStage({
  session,
  onGoToCreateDatapack,
}: {
  session: ReturnType<typeof useLabRunnerSession>;
  onGoToCreateDatapack: () => void;
}) {
  if (!session.datapackId) {
    return <LabStageNeedsDatapack onGoToCreateDatapack={onGoToCreateDatapack} />;
  }
  if (!isAmpEnabled()) {
    return (
      <p className="p-6 text-sm text-muted-foreground">
        AMP isn't enabled in this environment yet. Ask an admin to turn on the
        <code className="mx-1 font-mono">SHOW_PG_AMP</code> runtime flag.
      </p>
    );
  }
  return <CreateAmpClusterStageContent datapackId={session.datapackId} session={session} />;
}

function CreateAmpClusterStageContent({
  datapackId,
  session,
}: {
  datapackId: string;
  session: ReturnType<typeof useLabRunnerSession>;
}) {
  const { data: clusters, isLoading } = useValidAmpCluster(datapackId, session);
  const createMut = useCreateCluster(datapackId);
  // Same proactive gate DatapackClustersPanel.tsx uses: grey the create
  // button (with an upgrade tooltip) once the account is already at its
  // per-tier vCPU cap, instead of only catching the 402 after the click.
  const budgetQuery = useAmpCoreBudget(datapackId);
  const atLimit = budgetQuery.data?.at_limit ?? false;
  // Manual toggle (the "New" button) OR automatic once the list resolves
  // empty -- computed from the live query result rather than seeded once at
  // mount, so a first-time visitor (clusters still loading, then resolving
  // to []) lands on the create form once it's actually known there's
  // nothing to pick from, not based on a stale snapshot from before the
  // list loaded.
  const [manualCreate, setManualCreate] = useState(false);
  const showCreate = manualCreate || (!isLoading && (clusters?.length ?? 0) === 0);
  const [name, setName] = useState("");
  const [error, setError] = useState<{ message: string; upgrade: boolean } | null>(null);

  const handleCreate = async () => {
    const trimmed = name.trim();
    if (!trimmed || createMut.isPending) return;
    setError(null);
    try {
      // Without an explicit cpu, api_server charges AMP's 2-core default
      // against the tier cap (free tier is capped at 500 milli-cores) --
      // DEFAULT_CPU is the same "never overshoots the Free cap" value
      // CreateClusterWizard/useBudgetedCompute already send for exactly
      // this reason (see resize.ts's own comment on it).
      const cluster = await createMut.mutateAsync({ name: trimmed, cpu: DEFAULT_CPU });
      session.selectAmpCluster(cluster.id, cluster.default_branch.branch_id);
      setName("");
      setManualCreate(false);
    } catch (err) {
      const upgradeMsg = ampCoreLimitMessage(err);
      setError(
        upgradeMsg
          ? { message: upgradeMsg, upgrade: true }
          : { message: getApiErrorMessage(err, "Could not create the cluster."), upgrade: false },
      );
    }
  };

  return (
    <div className="mx-auto flex w-full max-w-md flex-1 flex-col justify-center gap-4 overflow-y-auto p-6">
      <LabStageHeader
        eyebrow="Chapter 4 · Step 2"
        title="Create an AMP cluster"
        lede="Provision a real fractional Postgres instance for this datapack. It's your app's storage, not Meko's."
        tags={["AMP"]}
      />
      <div className="flex items-center gap-2">
        <Database className="h-5 w-5 text-primary" aria-hidden />
        <h2 className="text-lg font-semibold text-foreground">Create an AMP cluster</h2>
      </div>
      <p className="text-sm text-muted-foreground">
        A cluster is one real Postgres database, with a <code>main</code> branch and a
        read-write endpoint, provisioned for this datapack in under a minute.
      </p>

      {session.ampClusterId ? (
        <div className="flex items-center gap-2 rounded-md border border-emerald-500/30 bg-emerald-500/5 p-3 text-sm text-emerald-700 dark:text-emerald-300">
          Ready. Click Next to continue.
        </div>
      ) : showCreate ? (
        <div className="flex flex-col gap-3 rounded-md border border-border p-4">
          <Label htmlFor="create-amp-cluster-name">Cluster name</Label>
          <Input
            id="create-amp-cluster-name"
            autoFocus
            placeholder="e.g. review-metrics"
            value={name}
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void handleCreate();
            }}
          />
          {error && (
            <p className="flex items-center gap-1.5 text-sm text-destructive">
              <AlertCircle className="h-4 w-4 shrink-0" />
              {error.upgrade ? <CoreLimitNotice message={error.message} /> : error.message}
            </p>
          )}
          <div className="flex gap-2">
            {atLimit ? (
              // Radix tooltips don't fire on a disabled element, so the span
              // (not the Button) is the actual hover target.
              <Tooltip>
                <TooltipTrigger asChild>
                  <span className="inline-flex">
                    <Button disabled className="cursor-not-allowed">
                      <Plus className="h-4 w-4" />
                      Create cluster
                    </Button>
                  </span>
                </TooltipTrigger>
                <TooltipContent side="top" className="max-w-[260px] text-center">
                  You've reached your plan's vCPU limit across your datapacks. Upgrade to{" "}
                  {tierLabel(budgetQuery.data?.next_tier)} in Settings to add more compute.
                </TooltipContent>
              </Tooltip>
            ) : (
              <Button onClick={() => void handleCreate()} disabled={!name.trim() || createMut.isPending}>
                {createMut.isPending && <Loader2 className="h-4 w-4 animate-spin" />}
                {createMut.isPending ? "Creating…" : "Create cluster"}
              </Button>
            )}
            {(clusters?.length ?? 0) > 0 && (
              <Button variant="ghost" onClick={() => setManualCreate(false)}>
                Cancel
              </Button>
            )}
          </div>
        </div>
      ) : (
        <div className="flex flex-col gap-3 rounded-md border border-border p-4">
          <Label htmlFor="select-amp-cluster">Select a cluster</Label>
          <div className="flex items-center gap-2">
            <Select
              value={session.ampClusterId ?? undefined}
              onValueChange={(clusterId) => {
                const cluster = clusters?.find((c) => c.id === clusterId);
                if (cluster) session.selectAmpCluster(cluster.id, cluster.default_branch.branch_id);
              }}
              disabled={isLoading}
            >
              <SelectTrigger id="select-amp-cluster" className="flex-1" aria-label="Select cluster">
                <SelectValue
                  placeholder={isLoading ? "Loading clusters…" : "Select a cluster"}
                />
              </SelectTrigger>
              <SelectContent>
                {(clusters ?? []).map((cluster) => (
                  <SelectItem key={cluster.id} value={cluster.id}>
                    {cluster.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
            <Button variant="outline" className="gap-1" onClick={() => setManualCreate(true)}>
              <Plus className="h-4 w-4" />
              New
            </Button>
          </div>
        </div>
      )}
    </div>
  );
}
