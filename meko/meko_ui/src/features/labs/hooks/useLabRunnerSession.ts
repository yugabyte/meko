import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { createDatapack, getDatapackProvisionStatus, type CurrentUser } from "@/lib/api/mekoApi";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { useAccountUsage, ACCOUNT_USAGE_QUERY_KEY } from "@/hooks/useAccountUsage";
import { DATAPACKS_QUERY_KEY, useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { isDatapackGrantEligibleForWorkbenchLM } from "@/lib/datapackShareGrants";
import { blockedMessage, getFreeTierStatus } from "@/lib/freeTier";
import { notifyDatapacksListChanged } from "@/features/observe/observeRefresh";
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";

import { useDeploymentStatus, useSharedDeploymentStatus } from "./useDeployments";
import type { LabDefinition } from "../types";
import type { LessonTrace } from "../lib/lessonTrace";

export type DeploymentMode = "shared" | "dedicated";

function selectedDatapackStorageKey(labId: string): string {
  return `meko:lab-datapack:${labId}`;
}

function selectedDeploymentStorageKey(labId: string): string {
  return `meko:lab-deployment:${labId}`;
}

function selectedDeploymentModeStorageKey(labId: string): string {
  return `meko:lab-deployment-mode:${labId}`;
}

function selectedAmpClusterStorageKey(labId: string): string {
  return `meko:lab-amp-cluster:${labId}`;
}

function selectedAmpBranchStorageKey(labId: string): string {
  return `meko:lab-amp-branch:${labId}`;
}

/**
 * Resolves the datapack a lab's demo app runs against, and exposes the
 * pieces the stage components need: the datapack id/name, the MCP bearer
 * token (mirrors WorkbenchLMPage's own provision-status lookup), the
 * signed-in user (for CollaborateStage's sharing UI), and a refreshTick any
 * chat/upload turn can bump so the observation panel refetches immediately.
 *
 * The datapack itself is a deliberate, per-lab choice, not auto-created:
 * `datapackId` starts null (or resumes the last choice persisted per lab
 * id) and only the caller's own `selectDatapack`/`createAndSelectDatapack`
 * set it — there is no silent auto-select-first-datapack fallback, unlike
 * WorkbenchLMPage. `accessibleDatapacks` reuses the same write-access grant
 * filter WorkbenchLM uses (owner/maintainer/contributor, not viewer) since
 * this session also writes memories and KB uploads.
 */
/** Keyed by datapack as well as lab: a trace describes calls made against ONE
 *  datapack, so carrying it across a switch showed the new datapack's pane
 *  beside the old one's X-Ray. */
function lessonTraceStorageKey(labId: string, datapackId: string | null): string {
  return `meko:lab-lesson-trace:${labId}:${datapackId ?? "none"}`;
}

/** A trace whose shape no longer parses is simply absent — X-Ray falls back
 *  to its empty state rather than rendering half a row. */
function readLessonTrace(labId: string, datapackId: string | null): LessonTrace | null {
  const raw = getLocalItem(lessonTraceStorageKey(labId, datapackId));
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<LessonTrace>;
    if (typeof parsed.lessonTitle !== "string" || !Array.isArray(parsed.calls)) return null;
    return parsed as LessonTrace;
  } catch {
    return null;
  }
}

export function useLabRunnerSession(lab: LabDefinition) {
  const { data: currentUser } = useCurrentUser();
  const userId = currentUser?.account_id ?? null;
  const queryClient = useQueryClient();

  const { data: allDatapacks, isLoading: datapacksLoading } = useDatapacks();
  const accessibleDatapacks = useMemo(
    () => (allDatapacks ?? []).filter(isDatapackGrantEligibleForWorkbenchLM),
    [allDatapacks],
  );

  // A lab-created datapack counts against the account's quota like any
  // other -- Labs isn't exempt, same free-tier gate DatapacksOverviewPage's
  // "Add Datapack" button uses.
  const { data: accountUsage } = useAccountUsage();
  const hasHitDatapackCount = useMemo(
    () => getFreeTierStatus(accountUsage, currentUser?.user_type).hasHitDatapackCount,
    [accountUsage, currentUser?.user_type],
  );

  const [datapackId, setDatapackIdState] = useState<string | null>(() =>
    getLocalItem(selectedDatapackStorageKey(lab.id)),
  );
  const [creating, setCreating] = useState(false);
  const [userToken, setUserToken] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [refreshTick, setRefreshTick] = useState(0);

  const bumpRefreshTick = useCallback(() => setRefreshTick((t) => t + 1), []);

  // The X-Ray pane's data: the calls the most recently completed lesson made.
  // Persisted (per lab AND datapack) so a reload doesn't tell a user who just
  // indexed a document to "complete a lesson" for work they can see in the
  // pane above it. Read straight from storage on every render rather than
  // held in state, so switching datapack swaps the trace with it instead of
  // showing the previous datapack's calls beside the new one's contents.
  // Recorded alongside bumpRefreshTick, not instead of it: this carries no
  // signal about *what* to refetch.
  const [traceTick, setTraceTick] = useState(0);
  const lastLessonTrace = useMemo(
    () => readLessonTrace(lab.id, datapackId),
    // traceTick is the write signal; datapackId is what makes it swap.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [lab.id, datapackId, traceTick],
  );
  const recordLessonTrace = useCallback(
    (trace: LessonTrace) => {
      setLocalItem(lessonTraceStorageKey(lab.id, datapackId), JSON.stringify(trace));
      setTraceTick((t) => t + 1);
    },
    [lab.id, datapackId],
  );

  const selectDatapack = useCallback(
    (id: string) => {
      setDatapackIdState(id);
      setLocalItem(selectedDatapackStorageKey(lab.id), id);
    },
    [lab.id],
  );

  // A datapack this hook itself just created, whose id `selectDatapack`
  // set optimistically off the create response — see createAndSelectDatapack
  // below. The invalidateQueries there is what's supposed to bring it into
  // `allDatapacks`, but if that refetch fails or races a cached page, the
  // eviction effect below would otherwise see a not-yet-accessible id and
  // drop a datapack the server did create. Cleared once the list actually
  // contains it (or once a *different* datapack is selected).
  const justCreatedDatapackIdRef = useRef<string | null>(null);

  // A persisted choice from a previous visit may no longer be accessible
  // (deleted, share revoked) — drop it once the real list is in so the
  // picker falls back to "no selection" instead of silently pointing at
  // a datapack the user can't use.
  useEffect(() => {
    if (!allDatapacks || !datapackId) return;
    if (accessibleDatapacks.some((dp) => dp.datapack_id === datapackId)) {
      if (justCreatedDatapackIdRef.current === datapackId) justCreatedDatapackIdRef.current = null;
      return;
    }
    if (justCreatedDatapackIdRef.current === datapackId) return;
    setDatapackIdState(null);
    removeLocalItem(selectedDatapackStorageKey(lab.id));
  }, [allDatapacks, accessibleDatapacks, datapackId, lab.id]);

  const createAndSelectDatapack = useCallback(
    async (name: string) => {
      if (!userId) throw new Error("Not signed in");
      // The BE re-enforces this too, but failing fast here (matching
      // DatapacksOverviewPage's "Add Datapack" gate) avoids a wasted
      // round-trip and a confusing generic error.
      if (hasHitDatapackCount) throw new Error(blockedMessage("datapack", currentUser?.user_type));
      setCreating(true);
      try {
        const created = await createDatapack({ name, user_id: userId });
        notifyDatapacksListChanged(`create:${created.name}`);
        // Neither invalidation is awaited -- both back active queries on
        // this same page (useDatapacks/useAccountUsage above), so awaiting
        // either blocks this function on however long that refetch takes.
        // Selection doesn't actually depend on the list being fresh first:
        // justCreatedDatapackIdRef (below) exists precisely so the eviction
        // effect leaves this datapack alone even if its refetch is slow,
        // fails, or races a cached page. Account usage backs
        // hasHitDatapackCount (used above, and by CreateDatapackStage's
        // create button) but doesn't need to be fresh *this* instant either:
        // the BE re-enforces the cap on the next create attempt regardless
        // (see the comment on that check above) -- and it's known to be slow
        // (an unindexed full-scan, see langfuse_repo.go:655-657), which is
        // exactly what caused this function to hang before this fix.
        void queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] });
        void queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] });
        justCreatedDatapackIdRef.current = created.datapack_id;
        selectDatapack(created.datapack_id);
      } finally {
        setCreating(false);
      }
    },
    [userId, hasHitDatapackCount, currentUser?.user_type, queryClient, selectDatapack],
  );

  useEffect(() => {
    if (!datapackId) {
      setUserToken(null);
      // A stale-choice eviction (see above) or a deselect can land here
      // right after the previous datapack's provision-status fetch set an
      // error -- clear it too, or the banner outlives the datapack it
      // described.
      setError(null);
      return;
    }
    // Real cancellation (not just a `cancelled` flag guarding setState) --
    // matches every other getDatapackProvisionStatus call site
    // (WorkbenchLMPage, DatapacksOverviewPage, DatapackDetailPage,
    // DatapackConfigureWizardPage) so a fast datapack switch doesn't leave
    // a stale request racing the new one.
    const ac = new AbortController();
    setLoading(true);
    setError(null);
    (async () => {
      try {
        const provision = await getDatapackProvisionStatus(datapackId, { signal: ac.signal });
        setUserToken(provision.mcp_api_key || null);
      } catch (err) {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setError(err instanceof Error ? err.message : "Failed to load this lab's datapack");
      } finally {
        if (!ac.signal.aborted) setLoading(false);
      }
    })();
    return () => ac.abort();
  }, [datapackId]);

  const datapackName = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_id === datapackId)?.datapack_name ?? null,
    [accessibleDatapacks, datapackId],
  );
  // For LabObservationSlot -> WorkbenchLMLiveObservationPanel's "Promote to
  // Ko" gating (owner/maintainer only) -- without it every lab datapack
  // reads as contributor-or-below, wrongly disabling promotion.
  const datapackGrant = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_id === datapackId)?.grant ?? null,
    [accessibleDatapacks, datapackId],
  );

  // Deployment selection: same "explicit choice, persisted per lab"
  // pattern as datapackId above, not auto-selected. DeployAgentPanel calls
  // selectDeployment once a create/BYOC-confirm succeeds; useDeploymentStatus
  // (already built for DeploymentStatus.tsx's own polling) keeps it fresh
  // until it reaches a terminal status.
  const [deploymentId, setDeploymentIdState] = useState<string | null>(() =>
    getLocalItem(selectedDeploymentStorageKey(lab.id)),
  );
  const selectDeployment = useCallback(
    (id: string) => {
      setDeploymentIdState(id);
      setLocalItem(selectedDeploymentStorageKey(lab.id), id);
    },
    [lab.id],
  );
  // Lets DeployAgentPanel's "Try again" return to the picker after a
  // terminal failure — unlike the datapackId cleanup above, this is a
  // deliberate user action, not automatic, since auto-clearing the moment
  // status flips to "failed" would hide the error before it's read.
  const clearDeployment = useCallback(() => {
    setDeploymentIdState(null);
    removeLocalItem(selectedDeploymentStorageKey(lab.id));
  }, [lab.id]);

  // Which AgentK this lab run uses: the one pre-deployed shared instance
  // (default -- no deploy wait), or one the user deploys themselves.
  // Persisted per lab, same pattern as datapackId/deploymentId above.
  const [deploymentMode, setDeploymentModeState] = useState<DeploymentMode>(
    () => (getLocalItem(selectedDeploymentModeStorageKey(lab.id)) as DeploymentMode | null) ?? "shared",
  );
  const selectDeploymentMode = useCallback(
    (mode: DeploymentMode) => {
      setDeploymentModeState(mode);
      setLocalItem(selectedDeploymentModeStorageKey(lab.id), mode);
    },
    [lab.id],
  );

  // The datapack's AMP cluster/branch for Chapter 3's dashboard -- same
  // "explicit choice, persisted per lab" pattern as deploymentId above. A
  // datapack's AMP pool can hold more than one cluster, but this lab only
  // ever creates/uses one, so a single id pair (not a list) is all any
  // stage needs. Data-capture (writing a real review-run row into this
  // cluster) isn't wired up yet -- see review-dashboard's own stage
  // comment -- so today this just remembers which cluster the user
  // provisioned.
  const [ampClusterId, setAmpClusterIdState] = useState<string | null>(() =>
    getLocalItem(selectedAmpClusterStorageKey(lab.id)),
  );
  const [ampBranchId, setAmpBranchIdState] = useState<string | null>(() =>
    getLocalItem(selectedAmpBranchStorageKey(lab.id)),
  );
  const selectAmpCluster = useCallback(
    (clusterId: string, branchId: string) => {
      setAmpClusterIdState(clusterId);
      setAmpBranchIdState(branchId);
      setLocalItem(selectedAmpClusterStorageKey(lab.id), clusterId);
      setLocalItem(selectedAmpBranchStorageKey(lab.id), branchId);
    },
    [lab.id],
  );
  const clearAmpCluster = useCallback(() => {
    setAmpClusterIdState(null);
    setAmpBranchIdState(null);
    removeLocalItem(selectedAmpClusterStorageKey(lab.id));
    removeLocalItem(selectedAmpBranchStorageKey(lab.id));
  }, [lab.id]);

  // A cluster id only exists inside the datapack pool it was created in --
  // unlike deploymentId (one account-wide resource), it doesn't carry over
  // to a different datapack. So when datapackId actually CHANGES (a
  // selectDatapack switch, or the eviction effect above nulling it), the
  // persisted cluster choice is no longer meaningful and must be dropped
  // too, or CreateAmpClusterStage/ReviewDashboardStage would keep showing
  // "Ready"/a dashboard for a cluster that lives in the datapack the user
  // just left. Guarded on an actual change (not the initial mount, where
  // prevDatapackIdRef starts equal to datapackId) so a persisted cluster
  // choice for the SAME restored datapack survives a page reload.
  // This doesn't catch the cluster being deleted server-side while
  // datapackId stays the same -- see useValidAmpCluster for that case.
  const prevDatapackIdRef = useRef(datapackId);
  useEffect(() => {
    if (prevDatapackIdRef.current === datapackId) return;
    prevDatapackIdRef.current = datapackId;
    clearAmpCluster();
  }, [datapackId, clearAmpCluster]);

  const sharedAgentSlug = lab.demoApp.deployable?.agentSlug ?? null;
  const { data: dedicatedDeployment } = useDeploymentStatus(deploymentId);
  const { data: sharedDeployment } = useSharedDeploymentStatus(
    sharedAgentSlug,
    deploymentMode === "shared" && sharedAgentSlug !== null,
  );
  const deployment = deploymentMode === "shared" ? (sharedDeployment ?? null) : (dedicatedDeployment ?? null);
  const deploymentActive = deployment?.status === "active";

  return useMemo(
    () => ({
      userId,
      currentUser: currentUser ?? null,
      datapackId,
      datapackName,
      datapackGrant,
      accessibleDatapacks,
      datapacksLoading,
      selectDatapack,
      createAndSelectDatapack,
      hasHitDatapackCount,
      creating,
      userToken,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
      lastLessonTrace,
      recordLessonTrace,
      deploymentId,
      selectDeployment,
      clearDeployment,
      deploymentMode,
      selectDeploymentMode,
      deployment,
      deploymentActive,
      ampClusterId,
      ampBranchId,
      selectAmpCluster,
      clearAmpCluster,
    }),
    [
      userId,
      currentUser,
      datapackId,
      datapackName,
      datapackGrant,
      accessibleDatapacks,
      datapacksLoading,
      selectDatapack,
      createAndSelectDatapack,
      hasHitDatapackCount,
      creating,
      userToken,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
      lastLessonTrace,
      recordLessonTrace,
      deploymentId,
      selectDeployment,
      clearDeployment,
      deploymentMode,
      selectDeploymentMode,
      deployment,
      deploymentActive,
      ampClusterId,
      ampBranchId,
      selectAmpCluster,
      clearAmpCluster,
    ],
  );
}

export type { CurrentUser };
