import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  ChevronDown,
  Link as LinkIcon,
  Pencil,
  Share2,
  SlidersHorizontal,
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
  datapackDetailPath,
  datapackSharePath,
} from "@/app/routes/paths";
import { isDatapackWizardConfigureDone } from "@/lib/datapackConfigureStatus";
import {
  DATAPACKS_LIST_UPDATED_EVENT,
  deleteDatapack,
  getApiErrorMessage,
  getDatapack,
  getProvisionStatus,
  PROVISION_STATUS_CACHE_KEY,
  updateDatapack,
  type CurrentUser,
  type Datapack,
  type ProvisionStatus,
  type UpdateDatapackPayload,
} from "@/lib/mekoApi";
import { dispatchAppEvent } from "@/lib/browserEvents";
import { toast } from "@/hooks/use-toast";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import { useEnsureLangfuseProjectKeysForTechPreviewDatapack } from "@/lib/langfuseProjectKeysBootstrap";
import MemoryPromotionPanel from "@/features/datapacks/components/MemoryPromotionPanel";
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
import { DatapackDetailConnectTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailConnectTile";
import { DatapackDetailMembersTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailMembersTile";
// Connect + upload modals (credentials UI and FileUpload wrapper).
import {
  DatapackConnectDialog,
  DatapackDeleteConfirmDialog,
  DatapackEditNameDialog,
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
  const [isEditNameOpen, setIsEditNameOpen] = useState(false);
  const [isDeleteOpen, setIsDeleteOpen] = useState(false);

  const [wizardConfigureDone, setWizardConfigureDone] = useState(() =>
    isDatapackWizardConfigureDone(datapackName),
  );

  const [knowledgeBaseCount, setKnowledgeBaseCount] = useState(0);
  const [memoriesCount, setMemoriesCount] = useState(0);
  const [learningsCount, setLearningsCount] = useState(0);
  const [conversationsCount, setConversationsCount] = useState(0);

  useEnsureLangfuseProjectKeysForTechPreviewDatapack(datapackName);

  useEffect(() => {
    setWizardConfigureDone(isDatapackWizardConfigureDone(datapackName));
  }, [datapackName]);

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
          setConversationsCount(dp.conversations_count ?? 0);
        }
      } catch (e) {
        if (!cancelled) {
          setDatapack(null);
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
    if (!datapack?.datapack_id) return;
    const cache = readProvisionStatusCache();
    const cached = cache[datapackName];
    if (cached) {
      setProvisionStatus((prev) => prev ?? cached);
      return;
    }
    let cancelled = false;
    void getProvisionStatus()
      .then((details) => {
        if (cancelled) return;
        setProvisionStatus(details);
        const next = { ...readProvisionStatusCache(), [datapackName]: details };
        writeProvisionStatusCache(next);
      })
      .catch(() => { });
    return () => {
      cancelled = true;
    };
  }, [datapackName, datapack?.datapack_id]);

  // useEffect(() => {
  //   const ac = new AbortController();
  //   setKnowledgeBaseCount(0);
  //   const run = async () => {
  //     try {
  //       const list = await listKnowledgeBases(datapackName, { signal: ac.signal });
  //       if (ac.signal.aborted) return;
  //       setKnowledgeBaseCount(list.length);
  //     } catch {
  //       if (ac.signal.aborted) return;
  //       setKnowledgeBaseCount(0);
  //     }
  //   };
  //   void run();
  //   return () => ac.abort();
  // }, [datapackName]);

  useEffect(() => {
    if (!isConnectOpen) return;
    let mounted = true;
    const cache = readProvisionStatusCache();
    const cached = cache[datapackName];
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
        const details = await getProvisionStatus();
        if (!mounted) return;
        setProvisionStatus(details);
        const nextCache = readProvisionStatusCache();
        nextCache[datapackName] = details;
        writeProvisionStatusCache(nextCache);
      } catch (err) {
        if (!mounted) return;
        setProvisionStatus(null);
        const nextCache = readProvisionStatusCache();
        delete nextCache[datapackName];
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
  }, [isConnectOpen, datapackName]);

  const displayId = datapack?.datapack_id ?? "—";
  const createdLabel = formatDatapackCreatedAtUtc(datapack?.created_at);
  const isDefaultDatapack = datapackName === DEFAULT_ACCOUNT_DATAPACK_NAME;

  const handleDatapackEditSave = async ({ name, description }: { name: string; description: string }) => {
    const trimmedName = name.trim();
    const trimmedDesc = description.trim();
    const prevDesc = (datapack?.description ?? "").trim();

    if (!trimmedName) {
      throw new Error("Name is required.");
    }

    const payload: UpdateDatapackPayload = {};
    if (trimmedName !== datapackName) {
      payload.name = trimmedName;
    }
    if (trimmedDesc !== prevDesc) {
      payload.description = trimmedDesc;
    }

    if (Object.keys(payload).length === 0) {
      return;
    }

    await updateDatapack(datapackName, payload);

    const renamed = trimmedName !== datapackName;
    if (renamed) {
      try {
        const st = await getDatapack(trimmedName);
        if (st.datapack_name !== trimmedName) {
          throw new Error("rename mismatch");
        }
      } catch {
        throw new Error(
          "The server did not apply this rename. The API may not support changing the datapack name yet.",
        );
      }
      dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
      toast({
        title: "Datapack updated",
        description: `Renamed to "${trimmedName}".`,
      });
      navigate(datapackDetailPath(trimmedName), { replace: true });
      return;
    }

    const next = await getDatapack(datapackName);
    setDatapack(next);
    setKnowledgeBaseCount(next.knowledge_count ?? 0);
    setMemoriesCount(next.memory_count ?? 0);
    setLearningsCount(next.learnings_count ?? 0);
    setConversationsCount(next.conversations_count ?? 0);
    dispatchAppEvent(DATAPACKS_LIST_UPDATED_EVENT);
    toast({
      title: "Datapack updated",
      description: "Your changes were saved.",
    });
  };

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
          statusLoading={statusLoading}
          actions={
            <>
              <Button
                type="button"
                variant="outline"
                className="gap-2 border-border bg-background/80"
                onClick={() => navigate(datapackSharePath(datapackName))}
              >
                <Share2 className="h-4 w-4 shrink-0" aria-hidden />
                Share
              </Button>
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
              <DatapackEditNameDialog
                open={isEditNameOpen}
                onOpenChange={setIsEditNameOpen}
                currentName={datapackName}
                currentDescription={datapack?.description ?? ""}
                onSave={handleDatapackEditSave}
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
                  <DropdownMenuItem
                    className="gap-2 cursor-pointer focus:bg-muted/20"
                    onSelect={() => setIsUploadOpen(true)}
                  >
                    <Upload className="h-4 w-4 shrink-0" aria-hidden />
                    Add Knowledge
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    className="gap-2 cursor-pointer focus:bg-muted/20"
                    onSelect={() => setIsConnectOpen(true)}
                  >
                    <LinkIcon className="h-4 w-4 shrink-0" aria-hidden />
                    Connect
                  </DropdownMenuItem>
                  {!wizardConfigureDone ? (
                    <DropdownMenuItem
                      className="gap-2 cursor-pointer focus:bg-muted/20"
                      onSelect={() => navigate(datapackConfigurePath(datapackName))}
                    >
                      <SlidersHorizontal className="h-4 w-4 shrink-0" aria-hidden />
                      Configure
                    </DropdownMenuItem>
                  ) : null}
                  <DropdownMenuSeparator className="bg-border" />
                  <DropdownMenuItem
                    className="gap-2 cursor-pointer focus:bg-muted/20"
                    disabled={isDefaultDatapack}
                    title={isDefaultDatapack ? "The default account datapack cannot be renamed." : undefined}
                    onSelect={() => {
                      if (!isDefaultDatapack) setIsEditNameOpen(true);
                    }}
                  >
                    <Pencil className="h-4 w-4 shrink-0" aria-hidden />
                    Edit {datapackName}
                  </DropdownMenuItem>
                  <DropdownMenuItem
                    className="gap-2 cursor-pointer text-red-300 focus:bg-red-950/40 focus:text-red-200"
                    disabled={isDefaultDatapack}
                    title={isDefaultDatapack ? "The default account datapack cannot be deleted." : undefined}
                    onSelect={() => {
                      if (!isDefaultDatapack) setIsDeleteOpen(true);
                    }}
                  >
                    <Trash2 className="h-4 w-4 shrink-0" aria-hidden />
                    Delete {datapackName}
                  </DropdownMenuItem>
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
        />

        <div className="grid w-full grid-cols-1 gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,1fr)] lg:items-stretch lg:gap-6">
          <div className="flex min-h-0 min-w-0 flex-col">
            <DatapackDetailTabStrip activeTab={activeTab} onTabChange={setActiveTab} />
            <div className="mt-6 min-h-0 w-full flex-1">
              {activeTab === DATAPACK_DETAIL_TAB.learnings ? (
                datapack?.datapack_id ? (
                  <MemoryPromotionPanel
                    datapackName={datapackName}
                    datapackId={datapack.datapack_id}
                    variant="preview"
                  />
                ) : (
                  <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
                    {statusLoading ? "Loading datapack…" : "Datapack not available."}
                  </div>
                )
              ) : (
                <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
                  {DATAPACK_DETAIL_TAB_ITEMS.find((t) => t.id === activeTab)?.label} — content coming soon.
                </div>
              )}
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
