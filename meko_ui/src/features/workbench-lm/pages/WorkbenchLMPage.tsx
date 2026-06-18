// WorkbenchLM entry: header (datapack picker) + left knowledge panel + center
// chat (talks to the inference gateway) + right "live observation" rail.

import { useCallback, useEffect, useMemo, useState } from "react";
import { useSearchParams } from "react-router-dom";
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
  DATAPACKS_LIST_UPDATED_EVENT,
  getApiErrorMessage,
  getDatapackConversation,
  getDatapackProvisionStatus,
  listAgents,
  listDatapackConversations,
  type Agent,
  type DatapackConversation,
  type DatapackThreadMessage,
  type KnowledgeBaseDocument,
} from "@/lib/api/mekoApi";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { addAppEventListener, removeAppEventListener } from "@/lib/browserEvents";
import { getPersistedItem, removePersistedItem, setPersistedItem } from "@/lib/browserStorage";
import { isDatapackGrantEligibleForWorkbenchLM } from "@/lib/datapackShareGrants";
import { SAMPLE_KNOWLEDGE_FILENAME } from "@/features/workbench-lm/data/sampleKnowledge";
import { DATAPACK_ID_QUERY_PARAM } from "@/app/routes/paths";

// Optional dev override for the MCP bearer. When unset (the normal case), we
// fetch the token from /provision-status per datapack instead.
const DEV_MCP_TOKEN = (import.meta.env.VITE_MEKO_MCP_BEARER as string | undefined) ?? "";

// The URL is the source of truth; storage is the fallback for when the user lands on
// `/workbench-lm` with no query string (typical bookmark / nav-link case).

const DATAPACK_ID_STORAGE_KEY = "workbench-lm:selected-datapack-id";

// agent_id the inference gateway stamps on every WorkbenchLM conversation
// (mirrors `inference_gateway` config `DEFAULT_AGENT_ID`). The "previous
// chats" dropdown filters sessions to this agent so conversations written
// to the same datapack by other agents (CLI / MCP clients) don't appear.
const WORKBENCH_LM_AGENT_ID = "WorkbenchLM";

type Props = {
  /**
   * True only when `/workbench-lm` is the currently-routed path. Because
   * ProtectedRoutes keeps every page mounted (`display: none` toggle)
   * to preserve state across navigation, the page can't infer this from
   * its own lifecycle — it has to be told. When false, child polling
   * panels skip their intervals so the API isn't hit from a hidden tree.
   */
  isRouteActive: boolean;
};

export default function WorkbenchLMPage({ isRouteActive }: Props) {
  const [searchParams, setSearchParams] = useSearchParams();

  // Shared react-query cache (see `useDatapacks` for staleTime + invalidation
  // semantics). The hook returns `{ data, isLoading, error }` directly — we
  // rename via destructure to keep the rest of the file's local references
  // (datapacks / datapacksLoading / datapacksError) untouched.
  const {
    data: datapacks = [],
    isLoading: datapacksLoading,
    error: datapacksQueryError,
    refetch: refetchDatapacks,
  } = useDatapacks();
  const datapacksError = datapacksQueryError
    ? getApiErrorMessage(datapacksQueryError, "Failed to load datapacks.")
    : null;

  // `selectedId` is DERIVED from the URL query param (single source of
  // truth). Picking a datapack updates the URL via `setSearchParams`,
  // which triggers a re-render with the new derived value. The previous
  // two-state design (URL ⇄ useState mirror) caused an infinite render
  // loop: the mirror effects raced because each saw the OTHER side
  // stale and tried to "fix" it, ping-ponging the value forever.
  //
  // First-mount fallback: if the URL has no `datapack_id`, we read it
  // from the hybrid storage layer ONCE (`useMemo` with no deps), then
  // the auto-select effect below promotes that into the URL so future
  // renders all derive cleanly from `searchParams` alone.
  //
  // Same-name disambiguation rationale is unchanged — IDs are unique,
  // names are not.
  const initialStorageId = useMemo<string | null>(
    () => getPersistedItem(DATAPACK_ID_STORAGE_KEY),
    [],
  );
  const selectedId: string | null = useMemo(() => {
    const fromUrl = searchParams.get(DATAPACK_ID_QUERY_PARAM)?.trim();
    return fromUrl || initialStorageId;
  }, [searchParams, initialStorageId]);
  // Agents fetched for the selected datapack — kept in state for future use
  // (e.g. agent picker on the header).
  const [, setAgents] = useState<Agent[]>([]);
  // Account id is required by the gateway's `/chat` endpoint. Shared,
  // session-cached identity — one `/me` fetch app-wide.
  const { data: currentUser = null } = useCurrentUser();
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
  const [traceTarget, setTraceTarget] = useState<{
    messageId: string;
    conversationId: string;
  } | null>(null);

  // --- Sessions / resume-previous-chat state ------------------------------
  const SESSIONS_FETCH_LIMIT = 20;
  const [sessions, setSessions] = useState<DatapackConversation[]>([]);
  const [sessionsLoading, setSessionsLoading] = useState(false);
  const [loadedConversationId, setLoadedConversationId] = useState<string | null>(null);
  const [loadedThread, setLoadedThread] = useState<DatapackThreadMessage[] | null>(null);
  // Langfuse project id for the resumed conversation — captured from the
  // same getDatapackConversation response as `loadedThread` and forwarded
  // to the chat so each historical turn's DecisionTrace can deep-link.
  const [loadedLangfuseProjectId, setLoadedLangfuseProjectId] = useState<string | null>(null);

  // Datapacks the user can actually use in the WorkbenchLM: own them, OR
  // were shared in as maintainer / contributor. Viewers are excluded
  // because chat writes (memory_add, conversation_add_message) require
  // at least contributor — see `isDatapackGrantEligibleForWorkbenchLM`.
  const accessibleDatapacks = useMemo(
    () => datapacks.filter(isDatapackGrantEligibleForWorkbenchLM),
    [datapacks],
  );
  const selectedDatapack = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_id === selectedId) ?? null,
    [accessibleDatapacks, selectedId],
  );

  const selectedName = selectedDatapack?.datapack_name ?? null;

  // Bumped after each successful chat turn so the right-rail panels (memories
  // / shared knowledge / conversations) refetch from the server.
  const [refreshTick, setRefreshTick] = useState(0);

  // Reset our mirrored KB doc list + MCP token whenever the active datapack
  // changes so we don't reuse the previous datapack's state. Sessions /
  // loaded thread also reset — they're per-datapack scoped.
  useEffect(() => {
    setKbDocs(null);
    setMcpToken(null);
    setSessions([]);
    setLoadedConversationId(null);
    setLoadedThread(null);
    setLoadedLangfuseProjectId(null);
  }, [selectedDatapack?.datapack_id]);

  // Pull recent sessions for the active datapack — drives the dropdown.
  // Stable fetcher so both the datapack-change effect and the
  // "+ New chat" handler can reuse it. Errors are swallowed: a missing
  // sessions list shouldn't block the chat itself.
  const accountId = currentUser?.account_id ?? "";
  const refreshSessions = useCallback(
    async (signal?: AbortSignal) => {
      const datapackId = selectedDatapack?.datapack_id;
      if (!datapackId) {
        setSessions([]);
        return;
      }
      setSessionsLoading(true);
      try {
        const resp = await listDatapackConversations(
          datapackId,
          { limit: SESSIONS_FETCH_LIMIT, offset: 0 },
          { signal },
        );
        // Scope the dropdown to THIS user's WorkbenchLM sessions: the
        // datapack can be shared across members and written to by other
        // agents (CLI / MCP), so filter by agent_id + user_id. While the
        // cached identity is still resolving (`accountId === ""`) this
        // yields an empty list — deliberately, so we never flash other
        // members' sessions. The effect re-runs once `accountId` lands
        // (it's in this callback's deps) and fills the list.
        const conversations = (resp.conversations ?? []).filter(
          (c) => c.agent_id === WORKBENCH_LM_AGENT_ID && c.user_id === accountId,
        );
        setSessions(conversations);
      } catch (err) {
        if ((err as { name?: string })?.name === "CanceledError") return;
        // Non-fatal: the dropdown just stays empty. Logged for debugging.
        // eslint-disable-next-line no-console
        console.warn("Failed to load sessions list:", err);
        setSessions([]);
      } finally {
        setSessionsLoading(false);
      }
    },
    [selectedDatapack?.datapack_id, accountId],
  );

  // Fetch sessions on datapack change.
  useEffect(() => {
    if (!selectedName) return;
    const ac = new AbortController();
    void refreshSessions(ac.signal);
    return () => ac.abort();
  }, [selectedDatapack?.datapack_id, refreshSessions]);

  // Load a previous conversation into the chat. Fetches the full thread
  // (limit=100 covers the vast majority; threads beyond that page would
  // need "load older" which we can add later if anyone hits it).
  const handleSelectConversation = useCallback(
    async (conversationId: string) => {
      const datapackId = selectedDatapack?.datapack_id;
      if (!datapackId || !conversationId) return;
      try {
        const resp = await getDatapackConversation(
          datapackId,
          conversationId,
          { messagesLimit: 100, messagesOffset: 0 },
        );
        // Set the loaded id FIRST so the chat's useEffect that resets on
        // datapack/conversation changes sees the new value when it
        // processes the thread below.
        setLoadedConversationId(conversationId);
        setLoadedThread(resp.conversation.thread ?? []);
        setLoadedLangfuseProjectId(resp.langfuse_project_id ?? null);
      } catch (err) {
        // eslint-disable-next-line no-console
        console.warn("Failed to load conversation:", err);
      }
    },
    [selectedDatapack?.datapack_id],
  );

  // Start a fresh chat. Clears the loaded thread + id, then refetches
  // sessions so a conversation just completed (or one created from
  // another tab) shows up the next time the user opens the dropdown.
  const handleNewChat = useCallback(() => {
    setLoadedConversationId(null);
    setLoadedThread(null);
    setLoadedLangfuseProjectId(null);
    void refreshSessions();
  }, [refreshSessions]);

  // Pull the per-datapack mcp_api_key from /provision-status; the gateway
  // forwards it as `Authorization: Bearer <token>` to MCP. A failure here
  // leaves `mcpToken` null and the chat input falls back to the dev env var
  // (or stays disabled with a clear "MCP token unavailable" reason).
  useEffect(() => {
    if (!selectedId) {
      setMcpToken(null);
      return;
    }
    const ac = new AbortController();
    void getDatapackProvisionStatus(selectedId, { signal: ac.signal })
      .then((res) => setMcpToken(res.mcp_api_key || null))
      .catch((err) => {
        if ((err as { name?: string })?.name === "CanceledError") return;
        setMcpToken(null);
      });
    return () => ac.abort();
  }, [selectedId]);

  // Stable callback so `WorkbenchLMKnowledgePanel`'s `useEffect` deps don't
  // churn each render.
  const handleKbDocsChange = useCallback((docs: KnowledgeBaseDocument[]) => setKbDocs(docs), []);

  // Single mutator for the selection — writes to the URL, the derived
  // `selectedId` falls out of that on the next render. Centralised so
  // every call site (dropdown change, switch confirm, auto-select) goes
  // through one path, and there's only one place that ever calls
  // `setSearchParams`.
  //
  // Uses the functional-updater form of `setSearchParams` (analogous to
  // `setState((prev) => …)`) so the callback does NOT depend on the
  // current `searchParams` snapshot — `setSearchParams` itself is
  // stable per React Router, so `writeSelectedId` is now stable too.
  // That keeps child components receiving this callback (e.g.
  // `<WorkbenchLMHeader>`) from re-rendering whenever any other query
  // param changes.
  const writeSelectedId = useCallback(
    (nextId: string | null) => {
      setSearchParams(
        (prev) => {
          const next = new URLSearchParams(prev);
          if (nextId) {
            next.set(DATAPACK_ID_QUERY_PARAM, nextId);
          } else {
            next.delete(DATAPACK_ID_QUERY_PARAM);
          }
          return next;
        },
        { replace: true },
      );
    },
    [setSearchParams],
  );

  // Intercept the dropdown's onValueChange. Any real switch (i.e. user picks
  // a different datapack while one is already active) prompts for
  // confirmation — switching discards the in-process chat session, the
  // loaded provision-status / mcp_api_key, the KB doc list, and any draft.
  // The very first auto-selection on page load goes through the dedicated
  // default-select effect (bypasses this handler) so it stays silent.
  //
  // Takes a datapack ID (not name) because the dropdown is keyed by ID
  // now — two same-named datapacks would otherwise collide here.
  const handleSelectDatapack = useCallback(
    (newId: string) => {
      if (!newId || newId === selectedId) return;
      if (selectedId) {
        setPendingSwitch(newId);
        return;
      }
      writeSelectedId(newId);
    },
    [selectedId, writeSelectedId],
  );

  const confirmSwitch = useCallback(() => {
    if (pendingSwitch) {
      writeSelectedId(pendingSwitch);
    }
    setPendingSwitch(null);
  }, [pendingSwitch, writeSelectedId]);

  // Name lookup for the confirm dialog — the dialog wants a user-friendly
  // string, but `pendingSwitch` is an ID. Look up against the accessible
  // list (already filtered to packs the user can chat with).
  const pendingSwitchName = useMemo(
    () => accessibleDatapacks.find((dp) => dp.datapack_id === pendingSwitch)?.datapack_name ?? "",
    [accessibleDatapacks, pendingSwitch],
  );

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

  // ProtectedRoutes keeps WorkbenchLMPage permanently mounted (display
  // toggle), so a one-shot mount-time fetch wouldn't notice datapacks
  // created from /datapacks (or shared in) while the user was here.
  // notifyDatapacksListChanged() is fired by every page that mutates
  // the list (create / update / delete / share-accept). We trigger a
  // refetch on the cached query so this surface + every other surface
  // subscribed to `useDatapacks()` updates in one call.
  useEffect(() => {
    const onChanged = () => void refetchDatapacks();
    addAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, onChanged);
    return () => {
      removeAppEventListener(DATAPACKS_LIST_UPDATED_EVENT, onChanged);
    };
  }, [refetchDatapacks]);

  // Auto-select / validate the active datapack against the freshly
  // loaded list. Three cases:
  //   1. URL has no `datapack_id` and storage has no fallback (or it's
  //      stale) → pick the first accessible datapack.
  //   2. URL has a datapack_id that no longer matches any accessible
  //      row (deleted / share revoked / typo in a shared URL) → fall
  //      back to first accessible.
  //   3. URL's id is valid → no-op.
  //
  // The only write here is `writeSelectedId`, which goes through the
  // URL. There's no React state mirror, so no chance of the old
  // URL ⇄ state ping-pong loop.
  //
  // Owners come back first from the API today; if that changes we may
  // want to sort here to keep the user's own packs at the top.
  useEffect(() => {
    if (accessibleDatapacks.length === 0) return;
    if (selectedId && accessibleDatapacks.some((dp) => dp.datapack_id === selectedId)) {
      return;
    }
    writeSelectedId(accessibleDatapacks[0].datapack_id);
  }, [accessibleDatapacks, selectedId, writeSelectedId]);

  // One-way mirror to the hybrid storage so a fresh tab / reload
  // without the URL param still lands on the user's last datapack.
  // Pure side-effect — never feeds back into `selectedId`.
  useEffect(() => {
    if (selectedId) {
      setPersistedItem(DATAPACK_ID_STORAGE_KEY, selectedId);
    } else {
      removePersistedItem(DATAPACK_ID_STORAGE_KEY);
    }
  }, [selectedId]);

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

  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-hidden bg-background text-foreground">
      <AppWorkspaceHeader
        leading={
          <WorkbenchLMHeader
            accessibleDatapacks={accessibleDatapacks}
            selectedDatapackId={selectedId}
            onSelectDatapack={handleSelectDatapack}
            loading={datapacksLoading}
          />
        }
      />
      {datapacksError ? (
        <div className="border-b border-red-500/30 bg-red-500/10 px-6 py-2 text-xs text-red-700 dark:text-red-300">
          {datapacksError}
        </div>
      ) : null}
      <div className="grid min-h-0 flex-1 grid-cols-[320px_1fr_360px] overflow-hidden">
        <WorkbenchLMKnowledgePanel
          datapackName={selectedName}
          datapackId={selectedDatapack?.datapack_id ?? null}
          onDocumentsChange={handleKbDocsChange}
          paused={!isRouteActive}
          sessions={sessions}
          sessionsLoading={sessionsLoading}
          loadedConversationId={loadedConversationId}
          onSelectConversation={(id) => void handleSelectConversation(id)}
          onNewChat={handleNewChat}
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
          onOpenTrace={(messageId, conversationId) => setTraceTarget({ messageId, conversationId })}
          loadedConversationId={loadedConversationId}
          loadedThread={loadedThread}
          loadedLangfuseProjectId={loadedLangfuseProjectId}
        />
        <WorkbenchLMLiveObservationPanel
          datapackId={selectedDatapack?.datapack_id ?? null}
          datapackName={selectedName}
          refreshTick={refreshTick}
          // "Promote to Ko" for contributors (owner/maintainer keep it).
          datapackGrant={selectedDatapack?.grant ?? null}
          paused={!isRouteActive}
        />
      </div>

      <WorkbenchLMSwitchConfirmDialog
        open={pendingSwitch !== null}
        onOpenChange={(open) => {
          if (!open) setPendingSwitch(null);
        }}
        currentDatapackName={selectedName ?? ""}
        targetDatapackName={pendingSwitchName}
        onConfirm={confirmSwitch}
      />

      {/*
        Trace drawer — same component used by the Conversations viewer.
        It stays mounted while a target is set so the close animation can
        play; clearing `traceTarget` triggers the slide-out + unmount.
      */}
      {selectedDatapack?.datapack_id ? (
        <TraceDrawer
          open={traceTarget !== null}
          datapackId={selectedDatapack.datapack_id}
          conversationId={traceTarget?.conversationId ?? ""}
          messageId={traceTarget?.messageId ?? null}
          onClose={() => setTraceTarget(null)}
        />
      ) : null}
    </div>
  );
}
