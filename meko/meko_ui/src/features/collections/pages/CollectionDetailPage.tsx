// One collection: folders, the selected folder's datapacks, a panel describing
// the selection, and its settings.
//
// GET /collections/:id carries one row per datapack with its counts, so every
// folder total and the memory figures are sums the browser does, and selecting
// a row issues no request. Two more calls sit alongside it: /stats for the
// panel and /placements for datapack paths.

import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { FolderTree } from "lucide-react";
import { Skeleton } from "@/components/ui/skeleton";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from "@/components/ui/breadcrumb";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { ROUTES } from "@/app/routes/paths";
import { useCurrentUser } from "@/hooks/useCurrentUser";
import { useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { useCollection, useDatapackPaths } from "@/features/collections/hooks/useCollections";
import { CollectionRuleCards } from "@/features/collections/components/CollectionRuleCards";
import { CollectionSettingsTab } from "@/features/collections/components/CollectionSettingsTab";
import { CollectionBrowser } from "@/features/collections/components/browser/CollectionBrowser";
import { CollectionStatsPanel } from "@/features/collections/components/CollectionStatsPanel";
import { CollectionActivityTab } from "@/features/collections/components/CollectionActivityTab";
import { sumNodes } from "@/features/collections/lib/collectionTree";
import {
  countLabel,
  COLLECTION_TAB_ACTIVITY,
  COLLECTION_TAB_FOLDERS,
  COLLECTION_TAB_SETTINGS,
  COLLECTIONS_TITLE,
} from "@/features/collections/collectionsCopy";

type CollectionTab = "folders" | "activity" | "settings";

export default function CollectionDetailPage({ collectionId }: { collectionId: string }) {
  const navigate = useNavigate();
  const { data, isLoading, error } = useCollection(collectionId);
  const { data: currentUser = null } = useCurrentUser();
  const { data: allDatapacks = [] } = useDatapacks();
  const { data: datapackPaths = {} } = useDatapackPaths();
  // Controlled rather than defaultValue: the tab is read elsewhere on the
  // page, and a rename or add has to leave the Folders tab open.
  const [tab, setTab] = useState<CollectionTab>("folders");

  const collection = data?.collection;
  const description = (collection?.description ?? "").trim();
  const isOwner = Boolean(currentUser && collection && collection.user_id === currentUser.account_id);

  // The move and delete pickers offer only what this user owns: both write
  // through endpoints that refuse anything else, so offering more would be
  // offering a guaranteed error.
  const ownDatapacks = useMemo(
    () => allDatapacks.filter((dp) => currentUser && dp.user_id === currentUser.account_id),
    [allDatapacks, currentUser],
  );
  const totals = useMemo(() => sumNodes(data?.datapacks ?? []), [data?.datapacks]);

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-6 pb-16">
        <Breadcrumb>
          <BreadcrumbList>
            <BreadcrumbItem>
              <BreadcrumbLink
                href={ROUTES.collections}
                onClick={(e) => {
                  e.preventDefault();
                  navigate(ROUTES.collections);
                }}
              >
                {COLLECTIONS_TITLE}
              </BreadcrumbLink>
            </BreadcrumbItem>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbPage>{collection?.collection_name ?? "…"}</BreadcrumbPage>
            </BreadcrumbItem>
          </BreadcrumbList>
        </Breadcrumb>

        {error ? (
          <InlineErrorBanner>
            {getApiErrorMessage(error, "Failed to load this collection.")}
          </InlineErrorBanner>
        ) : null}

        {isLoading ? (
          <div className="space-y-4">
            <Skeleton className="h-16 w-full max-w-md rounded-lg" />
            <Skeleton className="h-64 w-full rounded-xl" />
          </div>
        ) : data && collection ? (
          <>
            <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
              <div className="flex flex-col gap-2">
                <div className="flex items-center gap-3">
                  <div
                    className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-brand/15 ring-1 ring-brand/20"
                    aria-hidden
                  >
                    <FolderTree className="h-5 w-5 text-brand" />
                  </div>
                  <h1 className="text-2xl font-semibold tracking-tight text-foreground">
                    {collection.collection_name}
                  </h1>
                </div>
                <p className="text-muted-foreground text-sm max-w-2xl leading-relaxed">
                  {description ? `${description} · ` : ""}
                  {countLabel(totals.datapacks, "datapack")} in{" "}
                  {countLabel(data.folders.length, "folder")} ·{" "}
                  {countLabel(data.members.people, "person", "people")} ·{" "}
                  {countLabel(data.members.agents, "agent")} active · owner{" "}
                  {collection.owner_name || "you"}
                </p>
              </div>

            </div>

            <Tabs
              value={tab}
              onValueChange={(v) => setTab(v as CollectionTab)}
              className="space-y-6"
            >
              <TabsList>
                <TabsTrigger value="folders">{COLLECTION_TAB_FOLDERS}</TabsTrigger>
                <TabsTrigger value="activity">{COLLECTION_TAB_ACTIVITY}</TabsTrigger>
                {/* Every control on Settings is the owner's. A viewer with
                    no action available does not need the tab. */}
                {isOwner ? (
                  <TabsTrigger value="settings">{COLLECTION_TAB_SETTINGS}</TabsTrigger>
                ) : null}
              </TabsList>

              <TabsContent value="folders" className="space-y-6">
                <CollectionBrowser
                  detail={data}
                  isOwner={isOwner}
                  ownDatapacks={ownDatapacks}
                  datapackPaths={datapackPaths}
                  learningsCount={totals.awaitingReview}
                />

                <CollectionStatsPanel collectionId={collectionId} />

                <CollectionRuleCards />
              </TabsContent>

              <TabsContent value="activity">
                <CollectionActivityTab collectionId={collectionId} totals={totals} />
              </TabsContent>

              {isOwner ? (
                <TabsContent value="settings">
                  <CollectionSettingsTab
                    detail={data}
                    ownDatapacks={ownDatapacks}
                    onCollectionDeleted={() => navigate(ROUTES.collections)}
                  />
                </TabsContent>
              ) : null}
            </Tabs>
          </>
        ) : null}
      </main>
    </div>
  );
}
