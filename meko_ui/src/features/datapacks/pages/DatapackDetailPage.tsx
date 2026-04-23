import { useEffect, useMemo, useState } from "react";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import {
  getApiErrorMessage,
  getDatapackStatus,
  getProvisionStatus,
  PROVISION_STATUS_CACHE_KEY,
  type DatapackStatus,
  type ProvisionStatus,
} from "@/lib/mekoApi";
import { formatDatapackCreatedAtUtc } from "@/lib/datapackDateFormat";
import { useEnsureLangfuseProjectKeysForTechPreviewDatapack } from "@/lib/langfuseProjectKeysBootstrap";
// Overview pipeline (ME → Collective → KO) for the Overview tab.
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
// Connect + upload modals (credentials UI and FileUpload wrapper).
import { DatapackConnectDialog, DatapackKnowledgeUploadDialog } from "@/features/datapacks/components/datapack-detail/DatapackDetailDialogs";
// Breadcrumbs, title, badges, description, metadata row, and hero action slot.
import { DatapackDetailHero } from "@/features/datapacks/components/datapack-detail/DatapackDetailHero";
// Tab strip driving which detail sub-view is shown.
import { DatapackDetailTabStrip } from "@/features/datapacks/components/datapack-detail/DatapackDetailTabStrip";
// Six-card metrics ribbon (placeholder counts until APIs exist).
import { DatapackMetricsRibbon } from "@/features/datapacks/components/datapack-detail/DatapackMetricsRibbon";
// Tab id union + ordered tab list for labels / “coming soon” fallback copy.
import type { DatapackDetailTab } from "@/features/datapacks/components/datapack-detail/types";
import { DATAPACK_DETAIL_TAB_ITEMS } from "@/features/datapacks/components/datapack-detail/types";
// Memory tab: list mem0 rows for this datapack and promote selected rows into the tenant KB.
import MemoryPromotionPanel from "@/features/datapacks/components/MemoryPromotionPanel";

type Props = {
  datapackName: string;
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

export default function DatapackDetailPage({ datapackName }: Props) {
  const [datapackStatus, setDatapackStatus] = useState<DatapackStatus | null>(null);
  const [statusLoading, setStatusLoading] = useState(true);
  const [statusError, setStatusError] = useState<string | null>(null);
  const [activeTab, setActiveTab] = useState<DatapackDetailTab>("overview");

  const [isConnectOpen, setIsConnectOpen] = useState(false);
  const [connectLoading, setConnectLoading] = useState(false);
  const [connectError, setConnectError] = useState<string | null>(null);
  const [provisionStatus, setProvisionStatus] = useState<ProvisionStatus | null>(null);

  const [isUploadOpen, setIsUploadOpen] = useState(false);

  useEnsureLangfuseProjectKeysForTechPreviewDatapack(datapackName);

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

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        {/* DatapackDetailHero: breadcrumbs, title/badges/description, ID/logical DB/created; actions = Connect + Upload */}
        <DatapackDetailHero
          datapackName={datapackName}
          displayId={displayId}
          createdLabel={createdLabel}
          statusLoading={statusLoading}
          actions={
            <>
              {/* DatapackConnectDialog: modal with MCP URL, API key, DB user/password/conn string + copy */}
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
              {/* DatapackKnowledgeUploadDialog: modal body uses FileUpload for this datapack */}
              <DatapackKnowledgeUploadDialog
                datapackName={datapackName}
                open={isUploadOpen}
                onOpenChange={setIsUploadOpen}
              />
            </>
          }
        />

        {statusError ? (
          <div className="rounded-md border border-amber-500/40 bg-amber-500/10 px-4 py-3 text-sm text-amber-100">
            {statusError}
          </div>
        ) : null}

        {/* DatapackMetricsRibbon: six summary stat cards (placeholder zeros) */}
        <DatapackMetricsRibbon datapackStatus={datapackStatus} />

        {/* DatapackDetailTabStrip: switches Overview vs other detail sections */}
        <DatapackDetailTabStrip activeTab={activeTab} onTabChange={setActiveTab} />

        {activeTab === "overview" ? (
          <>
            {/* CollectiveLearningPipeline: Overview-only ME → Collective → KO diagram + section header */}
            <CollectiveLearningPipeline memoryCount={0} collectiveCount={0} knowledgeCount={datapackStatus?.knowledge_base_count ?? 0} />
          </>
        ) : activeTab === "memory" ? (
          // Mount MemoryPromotionPanel only once we have the datapack UUID; it's required by listMemories/syncMemoriesToKB.
          datapackStatus?.id ? (
            <MemoryPromotionPanel datapackName={datapackName} datapackId={datapackStatus.id} />
          ) : (
            <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
              {statusLoading ? "Loading datapack…" : "Datapack not available."}
            </div>
          )
        ) : (
          <div className="rounded-xl border border-dashed border-border bg-muted/5 px-6 py-16 text-center text-sm text-muted-foreground">
            {/* DATAPACK_DETAIL_TAB_ITEMS: human-readable tab label for non-Overview placeholders */}
            {DATAPACK_DETAIL_TAB_ITEMS.find((t) => t.id === activeTab)?.label} — content coming soon.
          </div>
        )}
      </main>
    </div>
  );
}
