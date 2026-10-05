// Projects list: every project the caller can reach, owned or shared,
// with its folder and datapack counts, plus the entry point for creating one.

import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import { FolderTree, Plus } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { InlineErrorBanner } from "@/components/ui/inline-error-banner";
import { AppWorkspaceHeader } from "@/features/layout/AppWorkspaceHeader";
import { getApiErrorMessage } from "@/lib/api/mekoApi";
import { projectDetailPath } from "@/app/routes/paths";
import { useDatapackPaths, useProjects } from "@/features/projects/hooks/useProjects";
import { useDatapacks } from "@/features/datapacks/hooks/useDatapacks";
import { ProjectSection } from "@/features/projects/components/ProjectSection";
import { NewProjectDialog } from "@/features/projects/components/NewProjectDialog";
import { ProjectsEmptyState } from "@/features/projects/components/ProjectsEmptyState";
import { ProjectExplainerPanel } from "@/features/projects/components/ProjectExplainerPanel";
import { filterProjects } from "@/features/projects/lib/filterProjects";
import { projectLearningsByProject } from "@/features/projects/lib/projectLearnings";
import {
  PROJECT_SCOPE,
  emptyScopeMessage,
  scopedProjects,
  type ProjectScope,
} from "@/features/projects/lib/scopeProjects";
import {
  NEW_PROJECT_LABEL,
  PROJECTS_SUBTITLE,
  PROJECTS_TITLE,
} from "@/features/projects/projectsCopy";

/** Placeholder cards while the list loads — enough to fill the first row so
 *  the grid doesn't reflow when real data lands. */
const LOADING_CARD_COUNT = 3;

export default function ProjectsOverviewPage() {
  const navigate = useNavigate();
  const [isNewOpen, setIsNewOpen] = useState(false);
  const [searchQuery, setSearchQuery] = useState("");
  const [scope, setScope] = useState<ProjectScope>(PROJECT_SCOPE.all);
  const { data: projectLists, isLoading, error } = useProjects();
  const owned = projectLists?.owned ?? [];
  const shared = projectLists?.shared ?? [];
  // The per-project learnings rollup is derived from these two, both
  // already cached for the Datapacks page.
  const { data: datapacks = [] } = useDatapacks();
  const { data: datapackPaths = {} } = useDatapackPaths();

  const learningsByProject = useMemo(
    () => projectLearningsByProject(datapacks, datapackPaths),
    [datapacks, datapackPaths],
  );

  // Scope first, then search. Both run in the browser over data the page
  // already has: GET /projects carries folder_names, so matching a folder
  // costs no request.
  const matches = useMemo(
    () => filterProjects(scopedProjects(owned, shared, scope), searchQuery),
    [owned, shared, scope, searchQuery],
  );
  const searchActive = searchQuery.trim() !== "";
  const nothingAtAll = owned.length === 0 && shared.length === 0;

  // The busiest project, for the explainer's diagram. It needs at least one
  // FOLDER, not at least one datapack: the diagram is about the shape, and a
  // project with folders draws that fine while it is still empty. Without
  // folders there is nothing to draw and the built-in sample is better.
  const exampleProject = useMemo(() => {
    const busiest = owned.reduce<(typeof owned)[number] | undefined>(
      (best, p) => (best && best.datapack_count >= p.datapack_count ? best : p),
      undefined,
    );
    return busiest && (busiest.folder_names?.length ?? 0) > 0 ? busiest : undefined;
  }, [owned]);

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
                {PROJECTS_TITLE}
              </h1>
            </div>
            <p className="text-muted-foreground text-sm max-w-xl leading-relaxed">
              {PROJECTS_SUBTITLE}
            </p>
          </div>

          <Button
            type="button"
            size="lg"
            className="gap-2 shrink-0 font-medium"
            onClick={() => setIsNewOpen(true)}
          >
            <Plus className="h-4 w-4" />
            {NEW_PROJECT_LABEL}
          </Button>
        </div>

        {error ? (
          <InlineErrorBanner>
            {getApiErrorMessage(error, "Failed to load projects.")}
          </InlineErrorBanner>
        ) : null}

        <ProjectExplainerPanel example={exampleProject} />

        {isLoading ? (
          <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            {Array.from({ length: LOADING_CARD_COUNT }, (_, i) => (
              <Skeleton key={i} className="h-44 w-full rounded-xl" />
            ))}
          </div>
        ) : nothingAtAll && !searchActive ? (
          // Nothing anywhere is a different situation from an empty tab: the
          // user has no concept of a project yet, so explain rather than
          // show an empty grid with three tabs above it.
          <ProjectsEmptyState onCreate={() => setIsNewOpen(true)} />
        ) : (
          <ProjectSection
            matches={matches}
            learningsByProject={learningsByProject}
            scope={scope}
            onScopeChange={setScope}
            searchQuery={searchQuery}
            onSearchChange={setSearchQuery}
            emptyMessage={emptyScopeMessage(scope, searchActive)}
            onOpen={(p) => navigate(projectDetailPath(p.project_id))}
            onCreate={() => setIsNewOpen(true)}
          />
        )}
      </main>

      <NewProjectDialog
        open={isNewOpen}
        onOpenChange={setIsNewOpen}
        onCreated={(project) => navigate(projectDetailPath(project.project_id))}
      />
    </div>
  );
}
