// The Datapacks grid: search, the owned/shared scope tabs, the collection filter,
// and the tile grid itself. A container — the tile and the add-card are their
// own files under datapacks-list/.

import { useMemo, useState } from "react";
import { Search } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import { sortDatapacksByCreatedAtDesc } from "@/lib/datapackDateFormat";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";
import type { DatapackPathMap, Collection } from "@/lib/api/collectionsApi";
import { CollectionScopeFilter } from "@/features/collections/components/CollectionScopeFilter";
import {
  COLLECTION_FILTER_ALL,
  COLLECTION_FILTER_UNFILED,
  filterDatapacksByCollection,
} from "@/features/collections/lib/filterDatapacksByCollection";
import {
  OWNED,
  SHARED,
} from "@/features/datapacks/components/datapacks-list/datapackTileLabels";
import { DatapackTile } from "@/features/datapacks/components/datapacks-list/DatapackTile";
import { NewDatapackPlaceholderCard } from "@/features/datapacks/components/datapacks-list/NewDatapackPlaceholderCard";
import { DatapacksTable } from "@/features/datapacks/components/datapacks-list/DatapacksTable";
import { DatapacksFooterNotes } from "@/features/collections/components/DatapacksFooterNotes";

function filterByName(datapacks: Datapack[], q: string): Datapack[] {
  const trimmed = q.trim().toLowerCase();
  if (!trimmed) return datapacks;
  return datapacks.filter((dp) => dp.datapack_name.toLowerCase().includes(trimmed));
}

type ScopeTab = "all" | "owned" | "shared";

type Props = {
  datapacks: Datapack[];
  isLoading: boolean;
  currentUser: CurrentUser | null;
  onOpenDatapack: (datapack: Datapack) => void;
  onConnectDatapack: (datapack: Datapack) => void;
  onShareDatapack: (datapack: Datapack) => void;
  onNewDatapack: () => void;
  /** Collections feature gate. Off, this renders exactly as it did before
   *  collections existed: no path line, no collection filter, no filing action. */
  showCollections?: boolean;
  collections?: Collection[];
  /** True when the caller owns at least one collection. Filing needs somewhere to
   *  file INTO, and a shared collection is not somewhere you can put things. */
  canFile?: boolean;
  datapackPaths?: DatapackPathMap;
  onFileDatapack?: (datapack: Datapack) => void;
};

function emptyCopy(scope: ScopeTab, searchActive: boolean, collectionFilterActive: boolean): string {
  if (searchActive) {
    if (scope === "owned") return "No owned datapacks match your search.";
    if (scope === "shared") return "No shared datapacks match your search.";
    return "No datapacks match your search.";
  }
  if (collectionFilterActive) return "No datapacks in this collection yet.";
  if (scope === "owned") return "You don't have any datapacks you own yet.";
  if (scope === "shared") return "You don't have any shared datapacks yet.";
  return "No datapacks to show.";
}

export default function DatapacksTilesSection({
  datapacks,
  isLoading,
  currentUser,
  onOpenDatapack,
  onConnectDatapack,
  onShareDatapack,
  onNewDatapack,
  showCollections = false,
  collections = [],
  canFile = false,
  datapackPaths = {},
  onFileDatapack,
}: Props) {
  const [searchQuery, setSearchQuery] = useState("");
  const [scopeTab, setScopeTab] = useState<ScopeTab>("all");
  const [collectionFilter, setCollectionFilter] = useState<string>(COLLECTION_FILTER_ALL);

  // Collection first, then scope, then name: narrowing by location before
  // ownership keeps the scope counts consistent with what the grid shows.
  const scoped = useMemo(
    () =>
      showCollections
        ? filterDatapacksByCollection(datapacks, datapackPaths, collectionFilter)
        : datapacks,
    [showCollections, datapacks, datapackPaths, collectionFilter],
  );

  const visibleDatapacks = useMemo(() => {
    const byScope =
      scopeTab === "owned"
        ? scoped.filter((dp) => isDatapackListGrantOwner(dp))
        : scopeTab === "shared"
          ? scoped.filter((dp) => !isDatapackListGrantOwner(dp))
          : sortDatapacksByCreatedAtDesc(scoped);
    return filterByName(byScope, searchQuery);
  }, [scoped, scopeTab, searchQuery]);

  const searchActive = searchQuery.trim() !== "";
  const collectionFilterActive = showCollections && collectionFilter !== COLLECTION_FILTER_ALL;
  const listEmpty = !isLoading && datapacks.length > 0 && visibleDatapacks.length === 0;

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex min-w-0 flex-wrap items-baseline gap-2">
          <h2 className="text-lg font-semibold text-foreground">Your datapacks</h2>
          <span
            className="text-sm tabular-nums text-muted-foreground"
            aria-live="polite"
            aria-label={`${visibleDatapacks.length} datapacks in current view`}
          >
            {visibleDatapacks.length}
          </span>
        </div>

        <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center sm:justify-end lg:shrink-0">
          <Tabs
            value={scopeTab}
            onValueChange={(v) => setScopeTab(v as ScopeTab)}
            className="w-full sm:w-auto"
          >
            <TabsList
              variant="segmented"
              className="w-full flex-wrap justify-start sm:w-auto sm:flex-nowrap"
            >
              <TabsTrigger value="all" variant="segmented">
                All
              </TabsTrigger>
              <TabsTrigger value="owned" variant="segmented">
                Mine
              </TabsTrigger>
              <TabsTrigger value="shared" variant="segmented">
                Shared with me
              </TabsTrigger>
            </TabsList>
          </Tabs>

          {showCollections ? (
            <CollectionScopeFilter
              collections={collections}
              value={collectionFilter}
              onChange={setCollectionFilter}
            />
          ) : null}

          <div className="relative w-full sm:w-auto sm:min-w-[200px] sm:max-w-[240px]">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              placeholder="Search by name..."
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              className="h-9 w-full border-border bg-input pl-9 text-foreground placeholder:text-muted-foreground"
              aria-label="Search datapacks by name"
            />
          </div>
        </div>
      </div>

      {isLoading ? (
        <p className="text-sm text-muted-foreground">Loading datapacks…</p>
      ) : datapacks.length === 0 ? (
        <p className="text-sm text-muted-foreground">
          No datapacks yet. Add a datapack to get started.
        </p>
      ) : listEmpty ? (
        <p className="text-sm text-muted-foreground">
          {emptyCopy(scopeTab, searchActive, collectionFilterActive)}
        </p>
      ) : (
        <>
          {showCollections ? (
            <>
              <DatapacksTable
                datapacks={visibleDatapacks}
                datapackPaths={datapackPaths}
                currentUser={currentUser}
                onOpenDatapack={onOpenDatapack}
                // Nothing to file into means every row's action stays Open
                // rather than becoming a dead button.
                onFileDatapack={canFile ? onFileDatapack : undefined}
              />
              <DatapacksFooterNotes />
            </>
          ) : (
            // Collections off: the page is exactly what it was before.
            <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-3 gap-4 [&>*]:min-h-0">
              {visibleDatapacks.map((dp) => (
                <DatapackTile
                  key={dp.datapack_id}
                  dp={dp}
                  variant={isDatapackListGrantOwner(dp) ? OWNED : SHARED}
                  currentUser={currentUser}
                  onOpenDatapack={onOpenDatapack}
                  onConnectDatapack={onConnectDatapack}
                  onShareDatapack={onShareDatapack}
                />
              ))}
              <NewDatapackPlaceholderCard onClick={onNewDatapack} />
            </div>
          )}
        </>
      )}
    </div>
  );
}
