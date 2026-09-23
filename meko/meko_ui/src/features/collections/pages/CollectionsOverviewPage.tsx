// Collections list: every collection in the workspace with its folder and datapack
// counts, plus the entry point for creating one from a layout.

import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { FolderTree, Plus, Search } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { ROUTES, collectionDetailPath } from "@/app/routes/paths";
import { useDatapackPaths, useCollections } from "@/features/collections/hooks/useCollections";
import { useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { CollectionSection } from "@/features/collections/components/CollectionSection";
import { NewCollectionDialog } from "@/features/collections/components/NewCollectionDialog";
import { CollectionsEmptyState } from "@/features/collections/components/CollectionsEmptyState";
import { UnfiledDatapacksPanel } from "@/features/collections/components/UnfiledDatapacksPanel";
import { CollectionExplainerPanel } from "@/features/collections/components/CollectionExplainerPanel";
import { filterCollections } from "@/features/collections/lib/filterCollections";
import {
  collectionLearningsByCollection,
  unfiledDatapacks,
} from "@/features/collections/lib/collectionLearnings";
import {
  NEW_COLLECTION_LABEL,
  COLLECTIONS_SUBTITLE,
  COLLECTIONS_TITLE,
  SHARED_COLLECTIONS_SUBTITLE,
  SHARED_COLLECTIONS_TITLE,
} from "@/features/collections/collectionsCopy";

/** Placeholder cards while the list loads — enough to fill the first row so
 *  the grid doesn't reflow when real data lands. */
const LOADING_CARD_COUNT = 3;

export default function CollectionsOverviewPage() {
  const navigate = useNavigate();
  const [isNewOpen, setIsNewOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const { data: collectionLists, isLoading, error } = useCollections();
  const collections = collectionLists?.owned ?? [];
  const sharedCollections = collectionLists?.shared ?? [];
  // The unfiled panel and the per-collection learnings rollup are both derived
  // from these two, which are already cached for the Datapacks page.
  const { data: datapacks = [] } = useDatapacks();
  const { data: datapackPaths = {} } = useDatapackPaths();

  const learningsByCollection = useMemo(
    () => collectionLearningsByCollection(datapacks, datapackPaths),
    [datapacks, datapackPaths],
  );
  const unfiled = useMemo(
    () => unfiledDatapacks(datapacks, datapackPaths),
    [datapacks, datapackPaths],
  );
  // Search runs in the browser over data the page already has: GET /collections
  // carries folder_names, so matching a folder costs no request.
  const ownedMatches = useMemo(
    () => filterCollections(collections, searchQuery),
    [collections, searchQuery],
  );
  const sharedMatches = useMemo(
    () => filterCollections(sharedCollections, searchQuery),
    [sharedCollections, searchQuery],
  );
  const searchActive = searchQuery.trim() !== "";

  // The busiest collection, for the explainer's diagram. It needs at least one
  // FOLDER, not at least one datapack: the diagram is about the shape, and a
  // collection with folders draws that fine while it is still empty. Without
  // folders there is nothing to draw and the built-in sample is better.
  const exampleCollection = useMemo(() => {
    const busiest = collections.reduce<(typeof collections)[number] | undefined>(
      (best, p) => (best && best.datapack_count >= p.datapack_count ? best : p),
      undefined,
    );
    return busiest && (busiest.folder_names?.length ?? 0) > 0 ? busiest : undefined;
  }, [collections]);

  return (
    <div className="flex-1 h-screen overflow-y-auto bg-background text-foreground flex flex-col">
      <AppWorkspaceHeader />

      <main className="flex-1 p-8 max-w-6xl mx-auto w-full space-y-8 pb-16">
        <div className="flex flex-col sm:flex-row items-start sm:items-center justify-between gap-4">
          <div className="flex flex-col gap-2">
            <div className="flex items-center gap-3">
              <div
                className="flex h-10 w-10 shrink-0 items-center justify-center rounded-lg bg-brand/15 ring-1 ring-brand/20"
                aria-hidden
              >
                <FolderTree className="h-5 w-5 text-brand" />
              </div>
              <h1 className="text-2xl font-semibold tracking-tight text-foreground">
                {COLLECTIONS_TITLE}
              </h1>
            </div>
            <p className="text-muted-foreground text-sm max-w-xl leading-relaxed">
              {COLLECTIONS_SUBTITLE}
            </p>
          </div>

          <Button
            type="button"
            size="lg"
            className="gap-2 shrink-0 font-medium"
            onClick={() => setIsNewOpen(true)}
          >
            <Plus className="h-4 w-4" />
            {NEW_COLLECTION_LABEL}
          </Button>
        </div>

        {error ? (
          <InlineErrorBanner>{getApiErrorMessage(error, "Failed to load collections.")}</InlineErrorBanner>
        ) : null}

        <CollectionExplainerPanel example={exampleCollection} />

        <div className="relative w-full sm:max-w-xs">
          <Search
            className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            type="search"
            placeholder="Search collections and folders..."
            value={searchQuery}
            onChange={(e) => setSearchQuery(e.target.value)}
            className="h-9 w-full border-border bg-input pl-9 text-foreground placeholder:text-muted-foreground"
            aria-label="Search collections and folders by name"
          />
        </div>

        {isLoading ? (
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {Array.from({ length: LOADING_CARD_COUNT }, (_, i) => (
              <Skeleton key={i} className="h-44 w-full rounded-xl" />
            ))}
          </div>
        ) : collections.length === 0 && !searchActive ? (
          <CollectionsEmptyState onCreate={() => setIsNewOpen(true)} />
        ) : (
          <CollectionSection
            title="Your collections"
            matches={ownedMatches}
            learningsByCollection={learningsByCollection}
            emptyMessage={searchActive ? "No collections or folders match your search." : undefined}
            onOpen={(p) => navigate(collectionDetailPath(p.collection_id))}
          />
        )}

        <CollectionSection
          title={SHARED_COLLECTIONS_TITLE}
          subtitle={SHARED_COLLECTIONS_SUBTITLE}
          matches={sharedMatches}
          learningsByCollection={learningsByCollection}
          onOpen={(p) => navigate(collectionDetailPath(p.collection_id))}
        />

        <UnfiledDatapacksPanel
          datapacks={unfiled}
          onFileInto={() => navigate(ROUTES.datapacks)}
        />
      </main>

      <NewCollectionDialog
        open={isNewOpen}
        onOpenChange={setIsNewOpen}
        onCreated={(collection) => navigate(collectionDetailPath(collection.collection_id))}
      />
    </div>
  );
}
