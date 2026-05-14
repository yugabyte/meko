// WorkbenchLM entry: header (datapack picker) + left knowledge panel + center
// chat (talks to the inference gateway) + right "live observation" rail.

import { useCallback, useEffect, useMemo, useState } from "react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import {
  WorkbenchLMChat,
  type KnowledgeReadiness,
} from "@/features/workbench-lm/components/WorkbenchLMChat";
import { WorkbenchLMHeader } from "@/features/workbench-lm/components/WorkbenchLMHeader";
import { WorkbenchLMKnowledgePanel } from "@/features/workbench-lm/components/WorkbenchLMKnowledgePanel";
import { WorkbenchLMLiveObservationPanel } from "@/features/workbench-lm/components/WorkbenchLMLiveObservationPanel";
import { WorkbenchLMSwitchConfirmDialog } from "@/features/workbench-lm/components/WorkbenchLMSwitchConfirmDialog";
import { TraceDrawer } from "@/features/datapacks/components/conversations-viewer/TraceDrawer";
import {
  getApiErrorMessage,
  getCurrentUser,
  getDatapackProvisionStatus,
  listAgents,
  listDatapacks,
  type Agent,
  type CurrentUser,
  type Datapack,
  type KnowledgeBaseDocument,
} from "@/lib/mekoApi";
import { isDatapackGrantEligibleForWorkbenchLM } from "@/lib/datapackShareGrants";
import { SAMPLE_KNOWLEDGE_FILENAME } from "@/features/workbench-lm/data/sampleKnowledge";

// Optional dev override for the MCP bearer. When unset (the normal case), we
// fetch the token from /provision-status per datapack instead.
const DEV_MCP_TOKEN = (import.meta.env.VITE_MEKO_MCP_BEARER as string | undefined) ?? "";

type Props = {
  /**
   * True only when `/playground` is the currently-routed path. Because
   * ProtectedRoutes keeps every page mounted (`display: none` toggle)
   * to preserve state across navigation, the page can't infer this from
   * its own lifecycle — it has to be told. When false, child polling
   * panels skip their intervals so the API isn't hit from a hidden tree.
   */
  isRouteActive: boolean;
};

export default function WorkbenchLMPage({ isRouteActive }: Props) {
  const [datapacks, setDatapacks] = useState<Datapack[]>([]);
  const [datapacksLoading, setDatapacksLoading] = useState(true);
  const [datapacksError, setDatapacksError] = useState<string | null>(null);
  const [selectedName, setSelectedName] = useState<string | null>(null);
  // Agents fetched for the selected datapack — kept in state for future use
  // (e.g. agent picker on the header).
  const [, setAgents] = useState<Agent[]>([]);
  // Account id is required by the gateway's `/chat` endpoint; load once.
  const [currentUser, setCurrentUser] = useState<CurrentUser | null>(null);
  // KB docs are owned by `WorkbenchLMKnowledgePanel`; we mirror them here so
  // `WorkbenchLMChat` can gate the input until at least one doc is COMPLETED.
  // `null` (vs `[]`) distinguishes "not loaded yet" from "no docs."
  const [kbDocs, setKbDocs] = useState<KnowledgeBaseDocument[] | null>(null);
  // mcp_api_key resolved from /provision-status for the active datapack.
  // Reset whenever the datapack changes; refetched in the effect below.
  const [mcpToken, setMcpToken] = useState<string | null>(null);
  // The next datapack the user is trying to switch to, awaiting confirmation.
  const [pendingSwitch, setPendingSwitch] = useState<string | null>(null);
  // Trace drawer state — message_id + conversation_id to deep-link into the
  // existing TraceDrawer (same one the Conversations viewer uses). Owning it
  // at the page level so the drawer survives chat re-renders cleanly.
  const [traceTarget, setTraceTarget] = useState<{ messageId: string; conversationId: string } | null>(null);

  // Datapacks the user can actually use in the WorkbenchLM: own them, OR
  // were shared in as maintainer / contributor. Viewers are excluded
  // because chat writes (memory_add, conversation_add_message) require
  // at least contributor — see `isDatapackGrantEligibleForWorkbenchLM`.
  const accessibleDatapacks = useMemo(
    () => datapacks.filter(isDatapackGrantEligibleForWorkbenchLM),
    [datapacks],
  );
  const selectedDatapack = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_name === selectedName) ?? null,
    [accessibleDatapacks, selectedName],
  );

  // Bumped after each successful chat turn so the right-rail panels (memories
  // / shared knowledge / conversations) refetch from the server.
  const [refreshTick, setRefreshTick] = useState(0);

  // Reset our mirrored KB doc list + MCP token whenever the active datapack
  // changes so we don't reuse the previous datapack's state.
  useEffect(() => {
    setKbDocs(null);
    setMcpToken(null);
  }, [selectedDatapack?.datapack_id]);

  // Pull the per-datapack mcp_api_key from /provision-status; the gateway
  // forwards it as `Authorization: Bearer <token>` to MCP. A failure here
  // leaves `mcpToken` null and the chat input falls back to the dev env var
  // (or stays disabled with a clear "MCP token unavailable" reason).
  useEffect(() => {
    if (!selectedName) {
      setMcpToken(null);
      return;
    }
    const ac = new AbortController();
    void getDatapackProvisionStatus(selectedName, { signal: ac.signal })
      .then((res) => setMcpToken(res.mcp_api_key || null))
      .catch((err) => {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setMcpToken(null);
      });
    return () => ac.abort();
  }, [selectedName]);

  // Stable callback so `WorkbenchLMKnowledgePanel`'s `useEffect` deps don't
  // churn each render.
  const handleKbDocsChange = useCallback(
    (docs: KnowledgeBaseDocument[]) => setKbDocs(docs),
    [],
  );

  // Intercept the dropdown's onValueChange. Any real switch (i.e. user picks
  // a different datapack while one is already active) prompts for
  // confirmation — switching discards the in-process chat session, the
  // loaded provision-status / mcp_api_key, the KB doc list, and any draft.
  // The very first auto-selection on page load goes through the dedicated
  // default-select effect (bypasses this handler) so it stays silent.
  const handleSelectDatapack = useCallback(
    (newName: string) => {
      if (!newName || newName === selectedName) return;
      if (selectedName) {
        setPendingSwitch(newName);
        return;
      }
      setSelectedName(newName);
    },
    [selectedName],
  );

  const confirmSwitch = useCallback(() => {
    if (pendingSwitch) {
      setSelectedName(pendingSwitch);
    }
    setPendingSwitch(null);
  }, [pendingSwitch]);

  // Translate the doc list into a coarse readiness state the chat can act on.
  const knowledgeReadiness: KnowledgeReadiness = !selectedDatapack
    ? "absent"
    : kbDocs === null
      ? "loading"
      : kbDocs.length === 0
        ? "absent"
        : kbDocs.some((d) => d.status === "COMPLETED")
          ? "ready"
          : "processing";

  // True when the bundled travel sample is uploaded for the active
  // datapack. Drives the EmptyState — the example prompts are tailored
  // to that sample, so we only show them when it's actually loaded.
  const hasSampleKnowledge = useMemo(
    () =>
      (kbDocs ?? []).some((d) => {
        const filename = (d.document_name || "").split("/").pop() ?? "";
        // Tolerate the `<timestamp>_<name>` prefix the uploader prepends.
        return filename.replace(/^\d+_/, "") === SAMPLE_KNOWLEDGE_FILENAME;
      }),
    [kbDocs],
  );

  useEffect(() => {
    const ac = new AbortController();
    setDatapacksLoading(true);
    setDatapacksError(null);
    listDatapacks()
      .then((rows) => {
        setDatapacks(rows ?? []);
      })
      .catch((err) => {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setDatapacksError(getApiErrorMessage(err, "Failed to load datapacks."));
      })
      .finally(() => setDatapacksLoading(false));
    return () => ac.abort();
  }, []);

  // Default-select the first accessible datapack once they load. Owners
  // come back first from the API today; if that changes we may want to
  // sort here to keep the user's own packs at the top of the picker.
  useEffect(() => {
    if (selectedName) return;
    const first = accessibleDatapacks[0];
    if (first) setSelectedName(first.datapack_name);
  }, [accessibleDatapacks, selectedName]);

  // Fetch agents for the selected datapack (held for future use).
  useEffect(() => {
    if (!selectedName) {
      setAgents([]);
      return;
    }
    let cancelled = false;
    listAgents(selectedName)
      .then((rows) => {
        if (!cancelled) setAgents(rows ?? []);
      })
      .catch(() => {
        if (!cancelled) setAgents([]);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedName]);

  // Load the signed-in user once. The gateway requires user_id on every /chat
  // call; we cache it here instead of asking each chat turn to fetch.
  useEffect(() => {
    const ac = new AbortController();
    void getCurrentUser({ signal: ac.signal })
      .then((u) => setCurrentUser(u))
      .catch(() => setCurrentUser(null));
    return () => ac.abort();
  }, []);

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={
          <WorkbenchLMHeader
            accessibleDatapacks={accessibleDatapacks}
            selectedDatapackName={selectedName}
            onSelectDatapack={handleSelectDatapack}
            loading={datapacksLoading}
          />
        }
      />
      {datapacksError ? (
        <div className="border-b border-red-500/30 bg-red-500/10 px-6 py-2 text-xs text-red-300">
          {datapacksError}
        </div>
      ) : null}
      <div className="grid min-h-0 flex-1 grid-cols-[320px_1fr_360px] overflow-hidden">
        <WorkbenchLMKnowledgePanel
          datapackName={selectedName}
          onDocumentsChange={handleKbDocsChange}
          paused={!isRouteActive}
        />
        <WorkbenchLMChat
          datapackName={selectedName}
          datapackId={selectedDatapack?.datapack_id ?? null}
          userId={currentUser?.account_id ?? null}
          // Prefer the dev env override when set; otherwise fall through to
          // the per-datapack token from /provision-status.
          userToken={DEV_MCP_TOKEN || mcpToken}
          knowledgeReadiness={knowledgeReadiness}
          hasSampleKnowledge={hasSampleKnowledge}
          onTurnComplete={() => {
            setRefreshTick((t) => t + 1);
          }}
          onOpenTrace={(messageId, conversationId) =>
            setTraceTarget({ messageId, conversationId })
          }
        />
        <WorkbenchLMLiveObservationPanel
          datapackId={selectedDatapack?.datapack_id ?? null}
          datapackName={selectedName}
          refreshTick={refreshTick}
          paused={!isRouteActive}
        />
      </div>

      <WorkbenchLMSwitchConfirmDialog
        open={pendingSwitch !== null}
        onOpenChange={(open) => {
          if (!open) setPendingSwitch(null);
        }}
        currentDatapackName={selectedName ?? ""}
        targetDatapackName={pendingSwitch ?? ""}
        onConfirm={confirmSwitch}
      />

      {/*
        Trace drawer — same component used by the Conversations viewer.
        It stays mounted while a target is set so the close animation can
        play; clearing `traceTarget` triggers the slide-out + unmount.
      */}
      {selectedName ? (
        <TraceDrawer
          open={traceTarget !== null}
          datapackName={selectedName}
          datapackId={selectedDatapack?.datapack_id}
          conversationId={traceTarget?.conversationId ?? ""}
          messageId={traceTarget?.messageId ?? null}
          onClose={() => setTraceTarget(null)}
        />
      ) : null}
    </div>
  );
}
