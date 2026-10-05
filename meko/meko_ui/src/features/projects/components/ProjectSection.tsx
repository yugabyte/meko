// The projects grid: scope tabs, search, the cards, and the create tile
// that ends the grid.
//
// One grid rather than the stacked "Yours" / "Shared" sections it replaced.
// The Datapacks page already answers "whose is this" with tabs, and two lists
// on one page made a shared project read as a lesser kind of thing rather
// than the same thing owned by somebody else.

import { Folder, Search } from "lucide-react";
import { Input } from "@/components/ui/input";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { CreateTile } from "@/components/ui/create-tile";
import type { ProjectMatch } from "@/features/projects/lib/filterProjects";
import type { Project } from "@/lib/api/projectsApi";
import { ProjectCard } from "@/features/projects/components/ProjectCard";
import {
  PROJECT_SCOPE,
  type ProjectScope,
} from "@/features/projects/lib/scopeProjects";
import {
  PROJECTS_SEARCH_PLACEHOLDER,
  NEW_PROJECT_LABEL,
  NEW_PROJECT_TILE_BODY,
  SCOPE_TAB_ALL,
  SCOPE_TAB_MINE,
  SCOPE_TAB_SHARED,
} from "@/features/projects/projectsCopy";

export function ProjectSection({
  matches,
  learningsByProject,
  scope,
  onScopeChange,
  searchQuery,
  onSearchChange,
  emptyMessage,
  onOpen,
  onCreate,
}: {
  matches: ProjectMatch[];
  learningsByProject: Record<string, number>;
  scope: ProjectScope;
  onScopeChange: (scope: ProjectScope) => void;
  searchQuery: string;
  onSearchChange: (value: string) => void;
  emptyMessage: string;
  onOpen: (project: Project) => void;
  onCreate: () => void;
}) {
  return (
    <section className="space-y-4">
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <div className="flex min-w-0 flex-wrap items-baseline gap-2">
          <h2 className="text-lg font-semibold text-foreground">Your projects</h2>
          <span
            className="text-sm tabular-nums text-muted-foreground"
            aria-live="polite"
            aria-label={`${matches.length} projects in current view`}
          >
            {matches.length}
          </span>
        </div>

        <div className="flex flex-col gap-3 sm:flex-row sm:flex-wrap sm:items-center sm:justify-end lg:shrink-0">
          <Tabs
            value={scope}
            onValueChange={(v) => onScopeChange(v as ProjectScope)}
            className="w-full sm:w-auto"
          >
            <TabsList
              variant="segmented"
              className="w-full flex-wrap justify-start sm:w-auto sm:flex-nowrap"
            >
              <TabsTrigger value={PROJECT_SCOPE.all} variant="segmented">
                {SCOPE_TAB_ALL}
              </TabsTrigger>
              <TabsTrigger value={PROJECT_SCOPE.owned} variant="segmented">
                {SCOPE_TAB_MINE}
              </TabsTrigger>
              <TabsTrigger value={PROJECT_SCOPE.shared} variant="segmented">
                {SCOPE_TAB_SHARED}
              </TabsTrigger>
            </TabsList>
          </Tabs>

          <div className="relative w-full sm:w-auto sm:min-w-[200px] sm:max-w-[260px]">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
              aria-hidden
            />
            <Input
              type="search"
              placeholder={PROJECTS_SEARCH_PLACEHOLDER}
              value={searchQuery}
              onChange={(e) => onSearchChange(e.target.value)}
              className="h-9 w-full border-border bg-input pl-9 text-foreground placeholder:text-muted-foreground"
              aria-label="Search projects and folders by name"
            />
          </div>
        </div>
      </div>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {matches.map(({ project, matchedFolders }) => (
          <div key={project.project_id} className="space-y-1">
            <ProjectCard
              project={project}
              learningsCount={learningsByProject[project.project_id] ?? 0}
              onOpen={onOpen}
            />
            {/* Says WHY this card survived the search, so a match on a folder
                doesn't look like a match on the project name. */}
            {matchedFolders.length > 0 ? (
              <p className="flex items-center gap-1 px-1 text-xs text-muted-foreground">
                <Folder className="h-3 w-3 shrink-0" aria-hidden />
                matched {matchedFolders.join(", ")}
              </p>
            ) : null}
          </div>
        ))}

        {/* Not on Shared: that tab is other people's projects, and anything
            created here would land in yours, so the tile would be answering a
            question the tab is not asking. */}
        {scope !== PROJECT_SCOPE.shared ? (
          <CreateTile
            title={NEW_PROJECT_LABEL}
            description={NEW_PROJECT_TILE_BODY}
            onClick={onCreate}
          />
        ) : null}
      </div>

      {matches.length === 0 ? (
        <p className="text-sm text-muted-foreground">{emptyMessage}</p>
      ) : null}
    </section>
  );
}
