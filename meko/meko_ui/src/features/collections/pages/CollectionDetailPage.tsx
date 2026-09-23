// One collection: its shape as a tree, a panel describing whatever is selected,
// how memory becomes shared knowledge inside it, and its settings.
//
// A single GET /collections/:id feeds all of it. The response carries one row
// per datapack with its counts, so every folder total, the collection total and
// the learnings rollup are sums the browser does — selecting a row in the
// tree issues no request.

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
import { useCollection } from "@/features/collections/hooks/useCollections";
import { CollectionTree } from "@/features/collections/components/CollectionTree";
import { CollectionInspector } from "@/features/collections/components/CollectionInspector";
import { CollectiveLearningPipeline } from "@/features/datapacks/components/datapack-detail/CollectiveLearningPipeline";
import { CollectionRuleCards } from "@/features/collections/components/CollectionRuleCards";
import { CollectionSettingsTab } from "@/features/collections/components/CollectionSettingsTab";
import { CollectionFolderActions } from "@/features/collections/components/CollectionFolderActions";
import { CollectionActivityTab } from "@/features/collections/components/CollectionActivityTab";
import {
  COLLECTION_SELECTION,
  sumNodes,
  type TreeSelection,
} from "@/features/collections/lib/collectionTree";
import {
  countLabel,
  PIPELINE_SUBTITLE,
  PIPELINE_TITLE,
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
  const [selection, setSelection] = useState<TreeSelection>(COLLECTION_SELECTION);
  // Controlled rather than defaultValue: the header's folder buttons live
  // outside the Tabs and have to know which one is open.
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

              {/* Folders tab only. These act on the tree the tab shows, and
                  Settings offers the same pair in its own card, so carrying
                  them across every tab would duplicate the Settings ones and
                  strand them above an activity feed they cannot change. */}
              {tab === "folders" ? (
                <CollectionFolderActions
                  collectionId={collectionId}
                  folders={data.folders}
                  isOwner={isOwner}
                />
              ) : null}
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
                <div className="grid gap-4 lg:grid-cols-2 lg:items-start">
                  <CollectionTree
                    collectionName={collection.collection_name}
                    folders={data.folders}
                    datapacks={data.datapacks}
                    selection={selection}
                    onSelect={setSelection}
                  />
                  <CollectionInspector detail={data} selection={selection} />
                </div>

                {/* The datapack page's ribbon, minus Conversations. Their
                    count lives in Langfuse, one query per datapack, which is
                    the one thing on this page that would scale with collection
                    size. */}
                <CollectiveLearningPipeline
                  currentUser={currentUser}
                  title={PIPELINE_TITLE}
                  description={PIPELINE_SUBTITLE}
                  showConversations={false}
                  memoryCount={totals.memories}
                  learningsCount={totals.awaitingReview}
                  collectiveMemoryCount={totals.promoted}
                  knowledgeFileCount={totals.knowledge}
                  knowledgeFileAndPromotedCount={totals.knowledge + totals.promoted}
                  datapackId={null}
                />
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
