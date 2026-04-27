import { useEffect, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { Share2, SlidersHorizontal } from "lucide-react";
import { Button } from "@/components/ui/button";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { datapackConfigurePath, datapackSharePath } from "@/app/routes/paths";
import { isDatapackWizardConfigureDone } from "@/lib/datapackConfigureStatus";
import {
  getApiErrorMessage,
  getDatapackStatus,
  getProvisionStatus,
  listKnowledgeBases,
  PROVISION_STATUS_CACHE_KEY,
  type CurrentUser,
  type DatapackStatus,
  type ProvisionStatus,
} from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import { useEnsureLangfuseProjectKeysForTechPreviewDatapack } from "@/lib/langfuseProjectKeysBootstrap";
import MemoryPromotionPanel from "@/features/datapacks/components/MemoryPromotionPanel";
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
import { DatapackDetailMembersTile } from "@/features/datapacks/components/datapack-detail/DatapackDetailMembersTile";
// Connect + upload modals (credentials UI and FileUpload wrapper).
import { DatapackConnectDialog, DatapackKnowledgeUploadDialog } from "@/features/datapacks/components/datapack-detail/DatapackDetailDialogs";
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
  const [datapackStatus, setDatapackStatus] = useState<DatapackStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<DatapackDetailTab>(DATAPACK_DETAIL_TAB.learnings);

  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [provisionStatus, setProvisionStatus] = useState<ProvisionStatus | null>(null);

  const [isUploadOpen, setIsUploadOpen] = useState(false);

  const [wizardConfigureDone, setWizardConfigureDone] = useState(() =>
    isDatapackWizardConfigureDone(datapackName),
  );

  const [knowledgeBaseCount, setKnowledgeBaseCount] = useState(0);

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
        const s = await getDatapackStatus(datapackName);
        if (!cancelled) setDatapackStatus(s);
      } catch (e) {
        if (!cancelled) {
          setDatapackStatus(null);
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
    setKnowledgeBaseCount(0);
    const run = async () => {
      try {
        const list = await listKnowledgeBases(datapackName, { signal: ac.signal });
        if (ac.signal.aborted) return;
        setKnowledgeBaseCount(list.length);
      } catch {
        if (ac.signal.aborted) return;
        setKnowledgeBaseCount(0);
      }
    };
    void run();
    return () => ac.abort();
  }, [datapackName]);

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

  const displayId = datapackStatus?.id ?? "—";
  const createdLabel = formatDatapackCreatedAtUtc(datapackStatus?.created_at);

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 pr-1 pt-6 pl-1 pb-3 max-w-7xl mx-auto w-full space-y-8">
        {/* DatapackDetailHero: breadcrumbs, title/badges/description, ID/created; actions = knowledge upload, connect, configure, share */}
        <DatapackDetailHero
          datapackName={datapackName}
          displayId={displayId}
          createdLabel={createdLabel}
          statusLoading={statusLoading}
          actions={
            <>
              {/* Order: Add Knowledge → Connect → Configure → Share */}
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
              {!wizardConfigureDone ? (
                <Button
                  type="button"
                  variant="outline"
                  className="gap-2 border-border bg-background/80"
                  onClick={() => navigate(datapackConfigurePath(datapackName))}
                >
                  <SlidersHorizontal className="h-4 w-4 shrink-0" aria-hidden />
                  Configure
                </Button>
              ) : null}
              <Button variant="outline" className="gap-2 border-border bg-background/80" asChild>
                <Link to={datapackSharePath(datapackName)}>
                  <Share2 className="h-4 w-4 shrink-0" aria-hidden />
                  Share
                </Link>
              </Button>
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
          conversationsCount={0}
          memoryCount={0}
          learningsCount={0}
          datapackId={datapackStatus?.id ?? null}
          knowledgeCount={knowledgeBaseCount}
          onSelectSection={setActiveTab}
        />

        <div className="grid w-full grid-cols-1 gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,1fr)] lg:items-stretch lg:gap-6">
          <div className="flex min-h-0 min-w-0 flex-col">
            <DatapackDetailTabStrip activeTab={activeTab} onTabChange={setActiveTab} />
            <div className="mt-6 min-h-0 w-full flex-1">
              {activeTab === DATAPACK_DETAIL_TAB.learnings ? (
                datapackStatus?.id ? (
                  <MemoryPromotionPanel
                    datapackName={datapackName}
                    datapackId={datapackStatus.id}
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
          <div className="flex min-h-0 min-w-0 flex-col lg:self-start lg:w-full">
            <DatapackDetailMembersTile datapackName={datapackName} currentUser={currentUser} />
          </div>
        </div>
      </main>
    </div>
  );
}
