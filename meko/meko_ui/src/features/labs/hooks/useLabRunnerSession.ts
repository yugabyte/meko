import { useCallback, useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";

import { createDatapack, getDatapackProvisionStatus, type CurrentUser } from "@/lib/api/mekoApi";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { DATAPACKS_QUERY_KEY, useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { isDatapackGrantEligibleForWorkbenchLM } from "@/lib/datapackShareGrants";
import { getLocalItem, removeLocalItem, setLocalItem } from "@/lib/browserStorage";

import type { LabDefinition } from "../types";

function selectedDatapackStorageKey(labId: string): string {
  return `meko:lab-datapack:${labId}`;
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

  // A persisted choice from a previous visit may no longer be accessible
  // (deleted, share revoked) — drop it once the real list is in so the
  // picker falls back to "no selection" instead of silently pointing at
  // a datapack the user can't use.
  useEffect(() => {
    if (!allDatapacks || !datapackId) return;
    if (!accessibleDatapacks.some((dp) => dp.datapack_id === datapackId)) {
      setDatapackIdState(null);
      removeLocalItem(selectedDatapackStorageKey(lab.id));
    }
  }, [allDatapacks, accessibleDatapacks, datapackId, lab.id]);

  const createAndSelectDatapack = useCallback(
    async (name: string) => {
      if (!userId) throw new Error("Not signed in");
      setCreating(true);
      try {
        const created = await createDatapack({ name, user_id: userId });
        await queryClient.invalidateQueries({ queryKey: [DATAPACKS_QUERY_KEY] });
        selectDatapack(created.datapack_id);
      } finally {
        setCreating(false);
      }
    },
    [userId, queryClient, selectDatapack],
  );

  useEffect(() => {
    if (!datapackId) {
      setUserToken(null);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    (async () => {
      try {
        const provision = await getDatapackProvisionStatus(datapackId);
        if (cancelled) return;
        setUserToken(provision.mcp_api_key || null);
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : "Failed to load this lab's datapack");
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [datapackId]);

  const datapackName = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_id === datapackId)?.datapack_name ?? null,
    [accessibleDatapacks, datapackId],
  );

  return useMemo(
    () => ({
      userId,
      currentUser: currentUser ?? null,
      datapackId,
      datapackName,
      accessibleDatapacks,
      datapacksLoading,
      selectDatapack,
      createAndSelectDatapack,
      creating,
      userToken,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
    }),
    [
      userId,
      currentUser,
      datapackId,
      datapackName,
      accessibleDatapacks,
      datapacksLoading,
      selectDatapack,
      createAndSelectDatapack,
      creating,
      userToken,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
    ],
  );
}

export type { CurrentUser };
