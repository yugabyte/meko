import { useCallback, useEffect, useMemo, useState } from "react";

import {
  createDatapack,
  getDatapackProvisionStatus,
  getKnowledgeBaseDocuments,
  listDatapacks,
} from "@/lib/api/mekoApi";
import { useCurrentUser } from "@/hooks/useCurrentUser";

import { ORG_CODING_STANDARDS_FILENAME } from "../data/adlcReviewAgentLab/orgCodingStandards";
import type { LabDefinition } from "../types";

/**
 * Resolves (or lazily creates) the persistent, per-account datapack a lab's
 * demo app runs against, and exposes the pieces PrReviewDemoApp /
 * LabObservationSlot need: the datapack id/name, the MCP bearer token
 * (mirrors WorkbenchLMPage's own provision-status lookup), whether the
 * org-standards fixture is already loaded, and a refreshTick any chat turn
 * can bump so the observation panel refetches immediately.
 *
 * Out of scope (see plan): per-user ephemeral provisioning/teardown — this
 * datapack is created once and reused on every run.
 */
export function useLabRunnerSession(lab: LabDefinition) {
  const { data: currentUser } = useCurrentUser();
  const userId = currentUser?.account_id ?? null;

  const [datapackId, setDatapackId] = useState<string | null>(null);
  const [userToken, setUserToken] = useState<string | null>(null);
  const [hasOrgStandards, setHasOrgStandards] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [refreshTick, setRefreshTick] = useState(0);

  const bumpRefreshTick = useCallback(() => setRefreshTick((t) => t + 1), []);

  const resolveDatapack = useCallback(async () => {
    if (!userId) return;
    setLoading(true);
    setError(null);
    try {
      const existing = await listDatapacks();
      const found = existing.find((dp) => dp.datapack_name === lab.demoApp.datapackName);
      const resolvedId = found
        ? found.datapack_id
        : (await createDatapack({ name: lab.demoApp.datapackName, user_id: userId })).datapack_id;
      setDatapackId(resolvedId);

      const [provision, kbDocs] = await Promise.all([
        getDatapackProvisionStatus(resolvedId),
        getKnowledgeBaseDocuments(resolvedId, { page_size: 200 }),
      ]);
      setUserToken(provision.mcp_api_key || null);
      setHasOrgStandards(
        kbDocs.items.some((d) => {
          const filename = (d.document_name || "").split("/").pop() ?? "";
          return filename.replace(/^\d+_/, "") === ORG_CODING_STANDARDS_FILENAME;
        }),
      );
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to set up this lab's datapack");
    } finally {
      setLoading(false);
    }
  }, [userId, lab.demoApp.datapackName]);

  useEffect(() => {
    void resolveDatapack();
  }, [resolveDatapack]);

  const refreshKnowledgeStatus = useCallback(async () => {
    if (!datapackId) return;
    try {
      const kbDocs = await getKnowledgeBaseDocuments(datapackId, { page_size: 200 });
      setHasOrgStandards(
        kbDocs.items.some((d) => {
          const filename = (d.document_name || "").split("/").pop() ?? "";
          return filename.replace(/^\d+_/, "") === ORG_CODING_STANDARDS_FILENAME;
        }),
      );
    } catch {
      // Best-effort — the "Load coding standards" button just stays visible.
    }
  }, [datapackId]);

  return useMemo(
    () => ({
      userId,
      datapackId,
      datapackName: lab.demoApp.datapackName,
      userToken,
      hasOrgStandards,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
      refreshKnowledgeStatus,
    }),
    [
      userId,
      datapackId,
      lab.demoApp.datapackName,
      userToken,
      hasOrgStandards,
      loading,
      error,
      refreshTick,
      bumpRefreshTick,
      refreshKnowledgeStatus,
    ],
  );
}
