// The Datapacks grid: search, the owned/shared scope tabs, the project filter,
// and the tile grid itself. A container — the tile and the add-card are their
// own files under datapacks-list/.

import { useMemo, useState } from "react";
import { Search } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import type { CurrentUser, Datapack } from "@/lib/api/mekoApi";
import { sortDatapacksByCreatedAtDesc } from "@/lib/datapackDateFormat";
import { isDatapackListGrantOwner } from "@/lib/datapackShareGrants";
import type { DatapackPathMap, Project } from "@/lib/api/projectsApi";
import { ProjectScopeFilter } from "@/features/projects/components/ProjectScopeFilter";
import {
  PROJECT_FILTER_ALL,
  PROJECT_FILTER_UNFILED,
  filterDatapacksByProject,
} from "@/features/projects/lib/filterDatapacksByProject";
import { OWNED, SHARED } from "@/features/datapacks/components/datapacks-list/datapackTileLabels";
import { DatapackTile } from "@/features/datapacks/components/datapacks-list/DatapackTile";
import { CreateTile } from "@/components/ui/create-tile";

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
  /** Projects feature gate. Off, this renders exactly as it did before
   *  projects existed: no path line, no project filter, no filing action. */
  showProjects?: boolean;
  projects?: Project[];
  /** True when the caller owns at least one project. Filing needs somewhere to
   *  file INTO, and a shared project is not somewhere you can put things. */
  canFile?: boolean;
  datapackPaths?: DatapackPathMap;
  onFileDatapack?: (datapack: Datapack) => void;
};

function emptyCopy(scope: ScopeTab, searchActive: boolean, projectFilterActive: boolean): string {
  if (searchActive) {
    if (scope === "owned") return "No owned datapacks match your search.";
    if (scope === "shared") return "No shared datapacks match your search.";
    return "No datapacks match your search.";
  }
  if (projectFilterActive) return "No datapacks in this project yet.";
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
  showProjects = false,
  projects = [],
  canFile = false,
  datapackPaths = {},
  onFileDatapack,
}: Props) {
  const [searchQuery, setSearchQuery] = useState("");
  const [scopeTab, setScopeTab] = useState<ScopeTab>("all");
  const [projectFilter, setProjectFilter] = useState<string>(PROJECT_FILTER_ALL);

  // Project first, then scope, then name: narrowing by location before
  // ownership keeps the scope counts consistent with what the grid shows.
  const scoped = useMemo(
    () =>
      showProjects ? filterDatapacksByProject(datapacks, datapackPaths, projectFilter) : datapacks,
    [showProjects, datapacks, datapackPaths, projectFilter],
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
  const projectFilterActive = showProjects && projectFilter !== PROJECT_FILTER_ALL;
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

          {showProjects ? (
            <ProjectScopeFilter
              projects={projects}
              value={projectFilter}
              onChange={setProjectFilter}
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
          {emptyCopy(scopeTab, searchActive, projectFilterActive)}
        </p>
      ) : (
        // One grid, flag on or off. The table this replaced answered "where
        // does it live" in a column; the tile answers it in a Project row
        // beside Access and Owner, because prefixing the name truncated it. A
        // second list shape for the same objects was what made the two pages
        // feel unrelated.
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
              path={datapackPaths[dp.datapack_id]}
              showPath={showProjects}
              // Nothing to file into means the slot keeps Connect rather than
              // becoming a dead button.
              onFileDatapack={canFile ? onFileDatapack : undefined}
            />
          ))}
          <CreateTile
            title="New datapack"
            description="Connect an existing project or start from scratch"
            onClick={onNewDatapack}
          />
        </div>
      )}
    </div>
  );
}
