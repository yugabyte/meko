import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { Pencil, Settings, Share2, Trash2, Upload } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { WarningBanner } from "@/components/ui/warning-banner";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { useAccountUsage } from "@/hooks/useAccountUsage";
import { FREE_TIER_BLOCKED_MESSAGES, getFreeTierStatus } from "@/lib/freeTier";
import { FreeTierLimitBanner } from "@/lib/freeTierBanner";
import {
  DEFAULT_ACCOUNT_DATAPACK_NAME,
  ROUTES,
  DATAPACK_QUERY_FLAG_ON,
  DATAPACK_QUERY_PARAM,
  datapackCollectiveMemorySummaryPath,
  datapackConfigurePath,
  datapackConversationsPath,
  datapackKnowledgePath,
  datapackLearningsPath,
  datapackMemorySummaryPath,
  datapackSharePath,
} from "@/app/routes/paths";
import {
  isDatapackGrantEligibleToAddKnowledgeBase,
  isDatapackGrantEligibleToDelete,
  isDatapackGrantEligibleToEdit,
  isDatapackGrantEligibleToShare
} from "@/lib/datapackShareGrants";
import {
  DatapackOutgoingMember,
  DatapackShareGrant,
  deleteDatapack,
  getApiErrorMessage,
  getDatapack,
  getDatapackConversationCount,
  getDatapackProvisionStatus,
  getKnowledgeBaseDocuments,
  getKnowledgeBaseSummary,
  isMekoApiRequestError,
  listDatapackMembers,
  PROVISION_STATUS_CACHE_KEY,
  type CurrentUser,
  type Datapack,
  type KnowledgeBaseDocument,
  type ProvisionStatus,
} from "@/lib/api/mekoApi";
import { notifyDatapacksListChanged } from "@/features/observe/observeRefresh";
import { toast } from "@/hooks/use-toast";
import { formatDatapackCreatedAtUtc, isDatapackCreatedWithinLast24Hours } from "@/lib/datapackDateFormat";
import { useEnsureLangfuseProjectKeysForTechPreviewDatapack } from "@/lib/langfuseProjectKeysBootstrap";
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
import { DatapackDetailAgentsTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailAgentsTile";
import { DatapackDetailConnectTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailConnectTile";
import {
  DatapackDetailKnowledgeTile,
  KNOWLEDGE_PREVIEW_LIMIT,
} from "@/features/datapacks/components/datapack-detail/DatapackDetailKnowledgeTile";
import {
  DatapackDetailMembersTile,
  type DatapackMembersListStatus,
} from "@/features/datapacks/components/datapack-detail/DatapackDetailMembersTile";
// Connect + upload modals (credentials UI and FileUpload wrapper).
import {
  DatapackConnectDialog,
  DatapackDeleteConfirmDialog,
  DatapackKnowledgeUploadDialog,
} from "@/features/datapacks/components/datapack-detail/dialogs";
// Breadcrumbs, title, badges, description, metadata row, and hero action slot.
import { DatapackDetailHero } from "@/features/datapacks/components/datapack-detail/DatapackDetailHero";
// Tab id union + ordered tab list for labels / “coming soon” fallback copy.
import { DATAPACK_DETAIL_TAB, type DatapackDetailTab } from "@/features/datapacks/components/datapack-detail/types";

type Props = {
  datapackId: string;
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

export default function DatapackDetailPage({ datapackId, currentUser }: Props) {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  const routeDatapackId = datapackId;
  const [datapack, setDatapack] = useState<Datapack | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  // Free-tier KB storage cap — gates the "Add Knowledge" dropdown item
  // below. Shared react-query cache with the rest of the app, so this
  // is a free read after any other page has fetched it.
  const { data: accountUsage } = useAccountUsage();
  const freeTier = getFreeTierStatus(accountUsage, currentUser?.user_type);

  // Each tile in CollectiveLearningPipeline routes directly to its dedicated
  // summary page — no inline preview list is rendered on the detail page.
  // Carries `routeDatapackId` through every navigation so the next page's
  // URL keeps the disambiguating `?datapack_id=` (name-collision safety).
  const handlePipelineSelect = useCallback(
    (tab: DatapackDetailTab) => {
      switch (tab) {
        case DATAPACK_DETAIL_TAB.conversations:
          navigate(datapackConversationsPath(routeDatapackId, undefined));
          return;
        case DATAPACK_DETAIL_TAB.memories:
          navigate(datapackMemorySummaryPath(routeDatapackId));
          return;
        case DATAPACK_DETAIL_TAB.learnings:
          navigate(datapackLearningsPath(routeDatapackId));
          return;
        case DATAPACK_DETAIL_TAB.collectiveMemory:
          navigate(datapackCollectiveMemorySummaryPath(routeDatapackId));
          return;
        case DATAPACK_DETAIL_TAB.knowledge:
          navigate(datapackKnowledgePath(routeDatapackId));
          return;
      }
    },
    [navigate, routeDatapackId],
  );

  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [provisionStatus, setProvisionStatus] = useState<ProvisionStatus | null>(null);

  const [isUploadOpen, setIsUploadOpen] = useState(false);
  const [isDeleteOpen, setIsDeleteOpen] = useState(false);

  // const [knowledgeBaseCount, setKnowledgeBaseCount] = useState(0);
  const [knowledgeDocuments, setKnowledgeDocuments] = useState<KnowledgeBaseDocument[]>([]);
  const [knowledgeDocumentsLoading, setKnowledgeDocumentsLoading] = useState(true);
  const [knowledgeDocumentsError, setKnowledgeDocumentsError] = useState<string | null>(null);
  const [knowledgeTotalCount, setKnowledgeTotalCount] = useState<number | null>(null);
  // True file count from the same summary fetch, surfaced separately
  // because the right-rail `<DatapackDetailKnowledgeTile>` chip is
  // file-only ("Knowledge · N files"), not files + promoted memories.
  // Splitting the two avoids overloading the meaning of either.
  const [knowledgeFileCount, setKnowledgeFileCount] = useState<number | null>(null);
  // Bumped after a successful upload (createKnowledgeBase) so the summary
  // effect re-fires and the file/promoted counts refresh without the
  // user having to navigate away and back.
  const [knowledgeSummaryRefreshTick, setKnowledgeSummaryRefreshTick] = useState(0);
  const [memoriesCount, setMemoriesCount] = useState(0);
  const [learningsCount, setLearningsCount] = useState(0);
  const [collectiveMemoryCount, setCollectiveMemoryCount] = useState(0);
  const [conversationsCount, setConversationsCount] = useState(0);
  const [datapackGrant, setDatapackGrant] = useState<DatapackShareGrant | null>(null);
  const [members, setMembers] = useState<DatapackOutgoingMember[]>([]);
  const [membersListStatus, setMembersListStatus] = useState<DatapackMembersListStatus>("loading");
  const displayDatapackName = datapack?.datapack_name ?? "";

  // Only the owner should provision project keys; null = not yet loaded.
  const isOwner = datapack !== null ? (datapackGrant === null || datapackGrant === "owner") : null;
  useEnsureLangfuseProjectKeysForTechPreviewDatapack(displayDatapackName, routeDatapackId, isOwner);

  useEffect(() => {
    if (searchParams.get(DATAPACK_QUERY_PARAM.connect) !== DATAPACK_QUERY_FLAG_ON) return;
    setIsConnectOpen(true);
    const next = new URLSearchParams(searchParams);
    next.delete(DATAPACK_QUERY_PARAM.connect);
    setSearchParams(next, { replace: true });
  }, [searchParams, setSearchParams]);

  useEffect(() => {
    let cancelled = false;
    const run = async () => {
      setStatusLoading(true);
      setStatusError(null);
      try {
        const dp = await getDatapack(routeDatapackId, {});
        if (!cancelled) {
          setDatapack(dp);
          // setKnowledgeBaseCount(dp.knowledge_count ?? 0);
          setMemoriesCount(dp.memory_count ?? 0);
          setCollectiveMemoryCount(dp.collective_memory_count ?? 0);
          setLearningsCount(dp.learnings_count ?? 0);
          setDatapackGrant(dp.grant ?? null);
        }
      } catch (e) {
        if (!cancelled) {
          if (
            isMekoApiRequestError(e) &&
            (e.httpStatus === 404 || e.code === "NOT_FOUND")
          ) {
            toast({
              title: "Datapack no longer available",
              description: `Datapack "${displayDatapackName}" was deleted or your access was removed.`,
            });
            navigate(ROUTES.datapacks, { replace: true });
            return;
          }
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
  }, [displayDatapackName, navigate, routeDatapackId]);

  useEffect(() => {
    const ac = new AbortController();
    setConversationsCount(0);
    void getDatapackConversationCount(routeDatapackId, { signal: ac.signal })
      .then((res) => {
        if (ac.signal.aborted) return;
        setConversationsCount(res.count ?? 0);
      })
      .catch(() => { });
    return () => {
      ac.abort();
    };
  }, [routeDatapackId]);

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
    void getDatapackProvisionStatus(routeDatapackId, { signal: ac.signal })
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
  }, [datapack?.datapack_id, routeDatapackId]);

  useEffect(() => {
    const ac = new AbortController();
    setMembersListStatus("loading");
    void listDatapackMembers(routeDatapackId, { signal: ac.signal })
      .then((res) => {
        if (ac.signal.aborted) return;
        setMembers(res.members ?? []);
        setMembersListStatus("ready");
      })
      .catch(() => {
        if (ac.signal.aborted) return;
        setMembers([]);
        setMembersListStatus("error");
      });
    return () => {
      ac.abort();
    };
  }, [datapack?.datapack_id, routeDatapackId]);

  useEffect(() => {
    const ac = new AbortController();
    setKnowledgeDocumentsLoading(true);
    setKnowledgeDocumentsError(null);
    // Fetch only what the right-rail preview tile actually renders
    // (top `KNOWLEDGE_PREVIEW_LIMIT` rows). Constant is exported from
    // the tile so the page-side fetch size and the tile-side slice
    // stay locked together. The Shared Knowledge tile's count no
    // longer depends on this fetch (it reads `getKnowledgeBaseSummary`
    // separately), so over-fetching here would be pure waste.
    void getKnowledgeBaseDocuments(routeDatapackId, { signal: ac.signal, page_size: KNOWLEDGE_PREVIEW_LIMIT })
      .then((page) => {
        if (ac.signal.aborted) return;
        setKnowledgeDocuments(page.items ?? []);
      })
      .catch((e) => {
        if (ac.signal.aborted) return;
        setKnowledgeDocuments([]);
        setKnowledgeDocumentsError(
          getApiErrorMessage(e, "Could not load knowledge documents."),
        );
      })
      .finally(() => {
        if (!ac.signal.aborted) setKnowledgeDocumentsLoading(false);
      });
    return () => ac.abort();
  }, [routeDatapackId]);

  // Knowledge base SUMMARY — count of promoted memories + successfukl file uploaded
  useEffect(() => {
    const ac = new AbortController();
    void getKnowledgeBaseSummary(routeDatapackId, { signal: ac.signal })
      .then((s) => {
        if (ac.signal.aborted) return;
        const files = s.total_files ?? 0;
        const promoted = s.promoted_memories_count ?? 0;
        setKnowledgeFileCount(files);
        setKnowledgeTotalCount(files + promoted);
      })
      .catch(() => {
        if (ac.signal.aborted) return;
        setKnowledgeFileCount(null);
        setKnowledgeTotalCount(null);
      });
    return () => ac.abort();
    // `knowledgeSummaryRefreshTick` is here so the upload dialog's
    // `onKnowledgeBaseCreated` callback re-runs this effect and the
    // counts update without a page reload.
  }, [routeDatapackId, knowledgeSummaryRefreshTick]);

  useEffect(() => {
    if (!isConnectOpen) return;
    const datapackId = datapack?.datapack_id;
    if (!datapackId) return;
    let mounted = true;

    // Always fetch fresh data — mcp_api_key is the caller's short-lived
    // access token, so a cached value would be stale.
    const loadProvisionStatus = async () => {
      try {
        setConnectLoading(true);
        setConnectError(null);
        const details = await getDatapackProvisionStatus(routeDatapackId, {});
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
  }, [isConnectOpen, datapack?.datapack_id, routeDatapackId]);

  const displayId = datapack?.datapack_id ?? "—";
  const createdLabel = formatDatapackCreatedAtUtc(datapack?.created_at);
  const isNewDatapack = isDatapackCreatedWithinLast24Hours(datapack?.created_at);
  const isDefaultDatapack = displayDatapackName === DEFAULT_ACCOUNT_DATAPACK_NAME;

  const showDatapackActionsDropdown =
    isDatapackGrantEligibleToAddKnowledgeBase({ grant: datapackGrant }) ||
    isDatapackGrantEligibleToEdit({ grant: datapackGrant }) ||
    isDatapackGrantEligibleToDelete({ grant: datapackGrant });
  const showActionsDropdownDangerSeparator =
    isDatapackGrantEligibleToAddKnowledgeBase({ grant: datapackGrant }) &&
    (isDatapackGrantEligibleToEdit({ grant: datapackGrant }) || isDatapackGrantEligibleToDelete({ grant: datapackGrant }));

  const handleDeleteConfirm = async () => {
    const { message } = await deleteDatapack(routeDatapackId, {});
    notifyDatapacksListChanged(`delete:${routeDatapackId}`);
    toast({
      title: "Datapack deleted",
      description: `"${displayDatapackName}" was deleted.`,
    });
    navigate(ROUTES.datapacks, { replace: true });
  };

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 pr-1 pt-6 pl-1 pb-3 max-w-7xl mx-auto w-full space-y-8">
        {freeTier.hasHitKbStorageFileSize ? (
          <FreeTierLimitBanner kind="kbStorage" />
        ) : null}

        {/* DatapackDetailHero: breadcrumbs, title/badges/description, ID/created; actions = Share (if eligible), upload dialog, settings menu (maintainer/owner only).
            Connect used to live up here but has been demoted to the body-level
            "Connect" tile + the high-level Connect CTA on /datapacks — Meko
            connection is conceptually a workspace-wide affordance, not a
            per-datapack one. */}
        <DatapackDetailHero
          datapackName={displayDatapackName}
          description={datapack?.description}
          displayId={displayId}
          createdLabel={createdLabel}
          datapackGrant={datapackGrant}
          isNewDatapack={isNewDatapack}
          statusLoading={statusLoading}
          actions={
            <>
              {isDatapackGrantEligibleToShare({ grant: datapackGrant }) && (
                <Button
                  type="button"
                  variant="outline"
                  className="gap-2 border-border bg-background/80"
                  onClick={() => navigate(datapackSharePath(routeDatapackId))}
                >
                  <Share2 className="h-4 w-4 shrink-0" aria-hidden />
                  Share
                </Button>
              )}
              <DatapackKnowledgeUploadDialog
                datapackName={displayDatapackName}
                datapackId={routeDatapackId}
                open={isUploadOpen}
                onOpenChange={setIsUploadOpen}
                onKnowledgeBaseCreated={() =>
                  setKnowledgeSummaryRefreshTick((t) => t + 1)
                }
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
                datapackName={displayDatapackName}
                onConfirm={handleDeleteConfirm}
              />
              {showDatapackActionsDropdown ? (
                <DropdownMenu>
                  <DropdownMenuTrigger asChild>
                    <Button
                      type="button"
                      variant="outline"
                      size="icon"
                      className="shrink-0 border-border bg-background/80"
                      aria-label="Datapack actions"
                    >
                      <Settings className="h-4 w-4 shrink-0 opacity-90" aria-hidden />
                    </Button>
                  </DropdownMenuTrigger>
                  <DropdownMenuContent align="end" className="min-w-[14rem] bg-card border-border text-foreground">
                    {isDatapackGrantEligibleToAddKnowledgeBase({ grant: datapackGrant }) && (
                      // Free-tier KB-storage cap: disable the item when the
                      // user has hit the byte limit. We can't render a
                      // floating tooltip on a DropdownMenuItem cleanly (the
                      // menu auto-closes on hover-out), so the disabled
                      // state + the appended muted-text hint conveys
                      // "you can't click this, here's why."
                      <DropdownMenuItem
                        className="gap-2 cursor-pointer focus:bg-muted/20 data-[disabled]:cursor-not-allowed"
                        onSelect={() => setIsUploadOpen(true)}
                        disabled={freeTier.hasHitKbStorageFileSize}
                        title={
                          freeTier.hasHitKbStorageFileSize
                            ? FREE_TIER_BLOCKED_MESSAGES.knowledgeUpload
                            : undefined
                        }
                      >
                        <Upload className="h-4 w-4 shrink-0" aria-hidden />
                        Add Knowledge
                        {freeTier.hasHitKbStorageFileSize ? (
                          <span className="ml-auto text-[10px] uppercase tracking-wider text-muted-foreground/70">
                            limit hit
                          </span>
                        ) : null}
                      </DropdownMenuItem>
                    )}
                    {showActionsDropdownDangerSeparator ? <DropdownMenuSeparator className="bg-border" /> : null}
                    {isDatapackGrantEligibleToEdit({ grant: datapackGrant }) && <DropdownMenuItem
                      className="gap-2 cursor-pointer focus:bg-muted/20"
                      onSelect={() => navigate(datapackConfigurePath(routeDatapackId))}
                    >
                      <Pencil className="h-4 w-4 shrink-0" aria-hidden />
                      Edit {displayDatapackName}
                    </DropdownMenuItem>}
                    {isDatapackGrantEligibleToDelete({ grant: datapackGrant }) && <DropdownMenuItem
                      className="gap-2 cursor-pointer text-red-700 dark:text-red-300 focus:bg-red-950/40 focus:text-red-700 dark:text-red-200"
                      // disabled={isDefaultDatapack}
                      // title={isDefaultDatapack ? "The default account datapack cannot be deleted." : undefined}
                      onSelect={() => {
                        if (!isDefaultDatapack) setIsDeleteOpen(true);
                      }}
                    >
                      <Trash2 className="h-4 w-4 shrink-0" aria-hidden />
                      Delete {displayDatapackName}
                    </DropdownMenuItem>}
                  </DropdownMenuContent>
                </DropdownMenu>
              ) : null}
            </>
          }
        />

        {statusError ? <WarningBanner>{statusError}</WarningBanner> : null}

        <CollectiveLearningPipeline
          currentUser={currentUser}
          conversationsCount={conversationsCount}
          memoryCount={memoriesCount}
          learningsCount={learningsCount}
          collectiveMemoryCount={collectiveMemoryCount}
          datapackId={datapack?.datapack_id ?? null}
          knowledgeFileCount={knowledgeFileCount ?? undefined}
          knowledgeFileAndPromotedCount={knowledgeTotalCount ?? undefined}
          onSelectSection={handlePipelineSelect}
          onUploadKnowledgeFiles={() => setIsUploadOpen(true)}
          statusLoading={statusLoading}
        />

        {/* Sidebar tiles — at-a-glance views; the pipeline above is the
            only path into the full Conversations / Memories / Learnings /
            Collective Memory / Knowledge summary pages. */}
        <div className="grid w-full grid-cols-1 gap-4 lg:grid-cols-2 lg:items-stretch lg:gap-6">
          <DatapackDetailMembersTile
            datapackName={displayDatapackName}
            datapackId={routeDatapackId}
            currentUser={currentUser}
            members={members}
            canShare={isDatapackGrantEligibleToShare({ grant: datapackGrant })}
            membersListStatus={membersListStatus}
          />
          <DatapackDetailKnowledgeTile
            datapackId={routeDatapackId}
            documents={knowledgeDocuments}
            documentsLoading={knowledgeDocumentsLoading}
            documentsError={knowledgeDocumentsError}
            totalFileCount={knowledgeFileCount}
            canAddSource={isDatapackGrantEligibleToAddKnowledgeBase({ grant: datapackGrant })}
            onAddSource={() => setIsUploadOpen(true)}
          />
          <DatapackDetailAgentsTile datapackId={routeDatapackId} />
          <DatapackDetailConnectTile
            mcpServerUrl={provisionStatus?.mcp_server_url ?? null}
            onOpenConnect={() => setIsConnectOpen(true)}
          />
        </div>
      </main>
    </div>
  );
}
