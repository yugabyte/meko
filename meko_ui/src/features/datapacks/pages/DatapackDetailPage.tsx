import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ChevronDown,
  Link as LinkIcon,
  Pencil,
  Share2,
  Trash2,
  Upload,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import {
  DEFAULT_ACCOUNT_DATAPACK_NAME,
  ROUTES,
  datapackConfigurePath,
  datapackSharePath,
} from "@/app/routes/paths";
import { DATAPACK_SHARE_GRANT_MAINTAINER, DATAPACK_SHARE_GRANT_OWNER } from "@/lib/datapackShareGrants";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  DatapackShareGrant,
  deleteDatapack,
  getApiErrorMessage,
  getDatapack,
  getDatapackConversationCount,
  getDatapackProvisionStatus,
  PROVISION_STATUS_CACHE_KEY,
  type CurrentUser,
  type Datapack,
  type ProvisionStatus,
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { toast } from "@/hooks/use-toast";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import { useEnsureLangfuseProjectKeysForTechPreviewDatapack } from "@/lib/langfuseProjectKeysBootstrap";
import MemoryPromotionPanel from "@/features/datapacks/components/MemoryPromotionPanel";
import { MemoryReadOnlySummaryPanel } from "@/features/datapacks/components/memory-promotion/MemoryReadOnlySummaryPanel";
import { KnowledgeDocumentsPanel } from "@/features/datapacks/components/knowledge-documents/KnowledgeDocumentsPanel";
import ConversationsPanel from "@/features/datapacks/components/datapack-detail/ConversationsPanel";
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
import { DatapackDetailConnectTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailConnectTile";
import { DatapackDetailMembersTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailMembersTile";
// Connect + upload modals (credentials UI and FileUpload wrapper).
import {
  DatapackConnectDialog,
  DatapackDeleteConfirmDialog,
  DatapackKnowledgeUploadDialog,
} from "@/features/datapacks/components/datapack-detail/dialogs";
// Breadcrumbs, title, badges, description, metadata row, and hero action slot.
import { DatapackDetailHero } from "@/features/datapacks/components/datapack-detail/DatapackDetailHero";
// Tab strip driving which detail sub-view is shown.
import { DatapackDetailTabStrip } from "@/features/datapacks/components/datapack-detail/DatapackDetailTabStrip";
// Tab id union + ordered tab list for labels / “coming soon” fallback copy.
import { DATAPACK_DETAIL_TAB, DATAPACK_DETAIL_TAB_ITEMS, type DatapackDetailTab } from "@/features/datapacks/components/datapack-detail/types";

type Props = {
  datapackName: string;
  currentUser: CurrentUser | null;
};

// Cache is keyed by datapack_id (stable UUID), not datapack_name, so renames
// don't orphan entries and cross-datapack lookups can never collide.
type ProvisionStatusCache = Record<string, ProvisionStatus>;

function readProvisionStatusCache(): ProvisionStatusCache {
  const raw = sessionStorage.getItem(PROVISION_STATUS_CACHE_KEY);
  if (!raw) return {};
  try {
    return JSON.parse(raw) as ProvisionStatusCache;
  } catch {
    sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
    return {};
  }
}

function writeProvisionStatusCache(cache: ProvisionStatusCache) {
  sessionStorage.setItem(PROVISION_STATUS_CACHE_KEY, JSON.stringify(cache));
}

export default function DatapackDetailPage({ datapackName, currentUser }: Props) {
  const navigate = useNavigate();
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<DatapackDetailTab>(DATAPACK_DETAIL_TAB.learnings);

  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [provisionStatus, setProvisionStatus] = useState<ProvisionStatus | null>(null);

  const [isUploadOpen, setIsUploadOpen] = useState(false);
  const [isDeleteOpen, setIsDeleteOpen] = useState(false);

  const [knowledgeBaseCount, setKnowledgeBaseCount] = useState(0);
  const [memoriesCount, setMemoriesCount] = useState(0);
  const [learningsCount, setLearningsCount] = useState(0);
  const [conversationsCount, setConversationsCount] = useState(0);
  const [datapackGrant, setDatapackGrant] = useState<DatapackShareGrant | null>(null);

  useEnsureLangfuseProjectKeysForTechPreviewDatapack(datapackName);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setStatusLoading(true);
      setStatusError(null);
      try {
        const dp = await getDatapack(datapackName);
        if (!cancelled) {
          setDatapack(dp);
          setKnowledgeBaseCount(dp.knowledge_count ?? 0);
          setMemoriesCount(dp.memory_count ?? 0);
          setLearningsCount(dp.learnings_count ?? 0);
          setDatapackGrant(dp.grant ?? null);
        }
      } catch (e) {
        if (!cancelled) {
          setDatapack(null);
          setDatapackGrant(null);
          setStatusError(getApiErrorMessage(e, "Failed to load datapack."));
        }
      } finally {
        if (!cancelled) setStatusLoading(false);
      }
    };
    void run();
    return () => {
      cancelled = true;
    };
  }, [datapackName]);

  useEffect(() => {
    const ac = new AbortController();
    setConversationsCount(0);
    void getDatapackConversationCount(datapackName, { signal: ac.signal })
      .then((res) => {
        if (ac.signal.aborted) return;
        setConversationsCount(res.count ?? 0);
      })
      .catch(() => { });
    return () => {
      ac.abort();
    };
  }, [datapackName]);

  useEffect(() => {
    const datapackId = datapack?.datapack_id;
    if (!datapackId) return;
    const cache = readProvisionStatusCache();
    const cached = cache[datapackId];
    if (cached) {
      setProvisionStatus(cached);
      return;
    }
    setProvisionStatus(null);
    const ac = new AbortController();
    void getDatapackProvisionStatus(datapackName, { signal: ac.signal })
      .then((details) => {
        if (ac.signal.aborted) return;
        setProvisionStatus(details);
        const next = { ...readProvisionStatusCache(), [datapackId]: details };
        writeProvisionStatusCache(next);
      })
      .catch(() => { });
    return () => {
      ac.abort();
    };
  }, [datapackName, datapack?.datapack_id]);

  useEffect(() => {
    if (!isConnectOpen) return;
    const datapackId = datapack?.datapack_id;
    if (!datapackId) return;
    let mounted = true;
    const cache = readProvisionStatusCache();
    const cached = cache[datapackId];
    if (cached && mounted) {
      setProvisionStatus(cached);
      setConnectLoading(false);
      setConnectError(null);
      return () => {
        mounted = false;
      };
    }

    const loadProvisionStatus = async () => {
      try {
        setConnectLoading(true);
        setConnectError(null);
        const details = await getDatapackProvisionStatus(datapackName);
        if (!mounted) return;
        setProvisionStatus(details);
        const nextCache = readProvisionStatusCache();
        nextCache[datapackId] = details;
        writeProvisionStatusCache(nextCache);
      } catch (err) {
        if (!mounted) return;
        setProvisionStatus(null);
        const nextCache = readProvisionStatusCache();
        delete nextCache[datapackId];
        if (Object.keys(nextCache).length === 0) {
          sessionStorage.removeItem(PROVISION_STATUS_CACHE_KEY);
        } else {
          writeProvisionStatusCache(nextCache);
        }
        setConnectError(getApiErrorMessage(err, "Failed to load connection details."));
      } finally {
        if (mounted) setConnectLoading(false);
      }
    };
    void loadProvisionStatus();
    return () => {
      mounted = false;
    };
  }, [isConnectOpen, datapackName, datapack?.datapack_id]);

  const displayId = datapack?.datapack_id ?? "—";
  const createdLabel = formatDatapackCreatedAtUtc(datapack?.created_at);
  const isDefaultDatapack = datapackName === DEFAULT_ACCOUNT_DATAPACK_NAME;

  /** Owner (no grant) or shared maintainer: can share, add knowledge, configure, edit, delete. */
  const canDatapackAdminActions =
    !statusLoading &&
    datapack != null &&
    (datapackGrant === DATAPACK_SHARE_GRANT_MAINTAINER || datapackGrant === DATAPACK_SHARE_GRANT_OWNER);

  const handleDeleteConfirm = async () => {
    const { message } = await deleteDatapack(datapackName);
    dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
    toast({
      title: "Datapack deleted",
      description: message || `"${datapackName}" was removed.`,
    });
    navigate(ROUTES.datapacks, { replace: true });
  };

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 pr-1 pt-6 pl-1 pb-3 max-w-7xl mx-auto w-full space-y-8">
        {/* DatapackDetailHero: breadcrumbs, title/badges/description, ID/created; actions = knowledge upload, connect, configure, share */}
        <DatapackDetailHero
          datapackName={datapackName}
          description={datapack?.description}
          displayId={displayId}
          createdLabel={createdLabel}
          datapackGrant={datapackGrant}
          statusLoading={statusLoading}
          actions={
            <>
              {canDatapackAdminActions ? (
                <Button
                  type="button"
                  variant="outline"
                  className="gap-2 border-border bg-background/80"
                  onClick={() => navigate(datapackSharePath(datapackName))}
                >
                  <Share2 className="h-4 w-4 shrink-0" aria-hidden />
                  Share
                </Button>
              ) : null}
              <DatapackKnowledgeUploadDialog
                datapackName={datapackName}
                open={isUploadOpen}
                onOpenChange={setIsUploadOpen}
              />
              <DatapackConnectDialog
                open={isConnectOpen}
                onOpenChange={(open) => {
                  setIsConnectOpen(open);
                  if (!open) {
                    setConnectError(null);
                    setConnectLoading(false);
                  }
                }}
                loading={connectLoading}
                error={connectError}
                provisionStatus={provisionStatus}
              />
              <DatapackDeleteConfirmDialog
                open={isDeleteOpen}
                onOpenChange={setIsDeleteOpen}
                datapackName={datapackName}
                onConfirm={handleDeleteConfirm}
              />
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button
                    type="button"
                    variant="outline"
                    className="gap-2 border-border bg-background/80"
                    aria-label="Datapack actions"
                  >
                    Actions
                    <ChevronDown className="h-4 w-4 shrink-0 opacity-70" aria-hidden />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="min-w-[14rem] bg-[#1C2333] border-border text-foreground">
                  {canDatapackAdminActions ? (
                    <DropdownMenuItem
                      className="gap-2 cursor-pointer focus:bg-muted/20"
                      onSelect={() => setIsUploadOpen(true)}
                    >
                      <Upload className="h-4 w-4 shrink-0" aria-hidden />
                      Add Knowledge
                    </DropdownMenuItem>
                  ) : null}
                  <DropdownMenuItem
                    className="gap-2 cursor-pointer focus:bg-muted/20"
                    onSelect={() => setIsConnectOpen(true)}
                  >
                    <LinkIcon className="h-4 w-4 shrink-0" aria-hidden />
                    Connect
                  </DropdownMenuItem>
                  {canDatapackAdminActions ? (
                    <>
                      <DropdownMenuSeparator className="bg-border" />
                      <DropdownMenuItem
                        className="gap-2 cursor-pointer focus:bg-muted/20"
                        onSelect={() => navigate(datapackConfigurePath(datapackName))}
                      >
                        <Pencil className="h-4 w-4 shrink-0" aria-hidden />
                        Edit {datapackName}
                      </DropdownMenuItem>
                      <DropdownMenuItem
                        className="gap-2 cursor-pointer text-red-300 focus:bg-red-950/40 focus:text-red-200"
                        // disabled={isDefaultDatapack}
                        // title={isDefaultDatapack ? "The default account datapack cannot be deleted." : undefined}
                        onSelect={() => {
                          if (!isDefaultDatapack) setIsDeleteOpen(true);
                        }}
                      >
                        <Trash2 className="h-4 w-4 shrink-0" aria-hidden />
                        Delete {datapackName}
                      </DropdownMenuItem>
                    </>
                  ) : null}
                </DropdownMenuContent>
              </DropdownMenu>
            </>
          }
        />

        {statusError ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-100">
            {statusError}
          </div>
        ) : null}

        <CollectiveLearningPipeline
          currentUser={currentUser}
          conversationsCount={conversationsCount}
          memoryCount={memoriesCount}
          learningsCount={learningsCount}
          datapackId={datapack?.datapack_id ?? null}
          knowledgeCount={knowledgeBaseCount}
          onSelectSection={setActiveTab}
          statusLoading={statusLoading}
        />

        <div className="grid w-full grid-cols-1 gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,1fr)] lg:items-stretch lg:gap-6">
          <div className="flex min-h-0 min-w-0 flex-col">
            <DatapackDetailTabStrip activeTab={activeTab} onTabChange={setActiveTab} />
            <div className="mt-6 min-h-0 w-full flex-1">
              {/*
                Conversations + Learnings panels stay MOUNTED across tab
                switches so their internal cache (current page, fetched
                rows) is preserved. Inactive ones are just hidden via
                CSS — no remount, no duplicate API calls. Memory and
                Knowledge tabs are still placeholder copy until those
                panels are built.
              */}
              <div hidden={activeTab !== DATAPACK_DETAIL_TAB.conversations}>
                <ConversationsPanel
                  datapackName={datapackName}
                  total={conversationsCount}
                />
              </div>
              <div hidden={activeTab !== DATAPACK_DETAIL_TAB.learnings}>
                {datapack?.datapack_id ? (
                  <MemoryPromotionPanel
                    datapackName={datapackName}
                    datapackId={datapack.datapack_id}
                    isPromoteAllowed={datapackGrant === DATAPACK_SHARE_GRANT_OWNER || datapackGrant === DATAPACK_SHARE_GRANT_MAINTAINER}
                    variant="preview"
                  />
                ) : (
                  <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
                    {statusLoading ? "Loading datapack…" : "Datapack not available."}
                  </div>
                )}
              </div>
              {activeTab === DATAPACK_DETAIL_TAB.knowledge ? (
                <KnowledgeDocumentsPanel
                  datapackName={datapackName}
                  knowledgeBaseCount={knowledgeBaseCount}
                  datapackSummaryReady={!statusLoading && datapack != null}
                />
              ) : activeTab === DATAPACK_DETAIL_TAB.memories ? (
                datapack?.datapack_id ? (
                  <MemoryReadOnlySummaryPanel
                    datapackName={datapackName}
                    datapackId={datapack.datapack_id}
                    variant="preview"
                  />
                ) : (
                  <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
                    {statusLoading ? "Loading datapack…" : "Datapack not available."}
                  </div>
                )
              ) : activeTab !== DATAPACK_DETAIL_TAB.conversations &&
                activeTab !== DATAPACK_DETAIL_TAB.learnings ? (
                <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
                  {DATAPACK_DETAIL_TAB_ITEMS.find((t) => t.id === activeTab)?.label} — content coming soon.
                </div>
              ) : null}
            </div>
          </div>
          <div className="mt-15 flex min-h-0 min-w-0 flex-col gap-4 lg:self-start lg:w-full">
            <DatapackDetailConnectTile
              mcpServerUrl={provisionStatus?.mcp_server_url ?? null}
              mcpApiKey={provisionStatus?.mcp_api_key ?? null}
              onOpenConnect={() => setIsConnectOpen(true)}
            />
            <DatapackDetailMembersTile datapackName={datapackName} currentUser={currentUser} />
          </div>
        </div>
      </main>
    </div>
  );
}
