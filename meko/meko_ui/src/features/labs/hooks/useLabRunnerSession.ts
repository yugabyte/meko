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

import { useDeploymentStatus } from "./useDeployments";
import type { LabDefinition } from "../types";

function selectedDatapackStorageKey(labId: string): string {
  return `meko:lab-datapack:${labId}`;
}

function selectedDeploymentStorageKey(labId: string): string {
  return `meko:lab-deployment:${labId}`;
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
        await Promise.all([
          queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] }),
          // Bumps datapack_count -- recompute free-tier gates immediately,
          // same as the canonical create-wizard call site.
          queryClient.invalidateQueries({ queryKey: [ACCOUNT_USAGE_QUERY_KEY] }),
        ]);
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
  const { data: deployment } = useDeploymentStatus(deploymentId);
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
      deploymentId,
      selectDeployment,
      clearDeployment,
      deployment: deployment ?? null,
      deploymentActive,
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
      deploymentId,
      selectDeployment,
      clearDeployment,
      deployment,
      deploymentActive,
    ],
  );
}

export type { CurrentUser };
