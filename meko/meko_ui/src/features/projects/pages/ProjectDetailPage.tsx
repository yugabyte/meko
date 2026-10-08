// One project: folders, the selected folder's datapacks, a panel describing
// the selection, and its settings.
//
// GET /projects/:id carries one row per datapack with its counts, so every
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
import { useProject, useDatapackPaths } from "@/features/projects/hooks/useProjects";
import { ProjectRuleCards } from "@/features/projects/components/ProjectRuleCards";
import { ProjectSettingsTab } from "@/features/projects/components/ProjectSettingsTab";
import { ProjectBrowser } from "@/features/projects/components/browser/ProjectBrowser";
import { ProjectStatsPanel } from "@/features/projects/components/ProjectStatsPanel";
import { ProjectActivityTab } from "@/features/projects/components/ProjectActivityTab";
import { sumNodes } from "@/features/projects/lib/projectTree";
import {
  countLabel,
  PROJECT_TAB_ACTIVITY,
  PROJECT_TAB_FOLDERS,
  PROJECT_TAB_SETTINGS,
  PROJECTS_TITLE,
} from "@/features/projects/projectsCopy";

type ProjectTab = "folders" | "activity" | "settings";

export default function ProjectDetailPage({ projectId }: { projectId: string }) {
  const navigate = useNavigate();
  const { data, isLoading, error } = useProject(projectId);
  const { data: currentUser = null } = useCurrentUser();
  const { data: allDatapacks = [] } = useDatapacks();
  const { data: datapackPaths = {} } = useDatapackPaths();
  // Controlled rather than defaultValue: the tab is read elsewhere on the
  // page, and a rename or add has to leave the Folders tab open.
  const [tab, setTab] = useState<ProjectTab>("folders");

  const project = data?.project;
  const description = (project?.description ?? "").trim();
  const isOwner = Boolean(currentUser && project && project.user_id === currentUser.account_id);

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
                href={ROUTES.projects}
                onClick={(e) => {
                  e.preventDefault();
                  navigate(ROUTES.projects);
                }}
              >
                {PROJECTS_TITLE}
              </BreadcrumbLink>
            </BreadcrumbItem>
            <BreadcrumbSeparator />
            <BreadcrumbItem>
              <BreadcrumbPage>{project?.project_name ?? "…"}</BreadcrumbPage>
            </BreadcrumbItem>
          </BreadcrumbList>
        </Breadcrumb>

        {error ? (
          <InlineErrorBanner>
            {getApiErrorMessage(error, "Failed to load this project.")}
          </InlineErrorBanner>
        ) : null}

        {isLoading ? (
          <div className="space-y-4">
            <Skeleton className="h-16 w-full max-w-md rounded-lg" />
            <Skeleton className="h-64 w-full rounded-xl" />
          </div>
        ) : data && project ? (
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
                    {project.project_name}
                  </h1>
                </div>
                <p className="text-muted-foreground text-sm max-w-2xl leading-relaxed">
                  {countLabel(totals.datapacks, "datapack")} in{" "}
                  {countLabel(data.folders.length, "folder")} ·{" "}
                  {countLabel(data.members.people, "person", "people")} ·{" "}
                  {countLabel(data.members.agents, "agent")} active · owner{" "}
                  {project.owner_name || "you"}
                </p>
              </div>

            </div>

            {/* On its own line rather than prefixed to the counts above,
                where a sentence wedged in front of "2 datapacks in 3 folders"
                read as the first item of the same list. */}
            {description ? (
              <p className="max-w-3xl text-sm leading-relaxed text-muted-foreground">
                {description}
              </p>
            ) : null}

            <Tabs
              value={tab}
              onValueChange={(v) => setTab(v as ProjectTab)}
              className="space-y-6"
            >
              <TabsList>
                <TabsTrigger value="folders">{PROJECT_TAB_FOLDERS}</TabsTrigger>
                <TabsTrigger value="activity">{PROJECT_TAB_ACTIVITY}</TabsTrigger>
                {/* Every control on Settings is the owner's. A viewer with
                    no action available does not need the tab. */}
                {isOwner ? (
                  <TabsTrigger value="settings">{PROJECT_TAB_SETTINGS}</TabsTrigger>
                ) : null}
              </TabsList>

              <TabsContent value="folders" className="space-y-6">
                <ProjectBrowser
                  detail={data}
                  isOwner={isOwner}
                  ownDatapacks={ownDatapacks}
                  datapackPaths={datapackPaths}
                  learningsCount={totals.awaitingReview}
                />

                <ProjectStatsPanel projectId={projectId} />

                <ProjectRuleCards />
              </TabsContent>

              <TabsContent value="activity">
                <ProjectActivityTab projectId={projectId} totals={totals} />
              </TabsContent>

              {isOwner ? (
                <TabsContent value="settings">
                  <ProjectSettingsTab
                    detail={data}
                    ownDatapacks={ownDatapacks}
                    onProjectDeleted={() => navigate(ROUTES.projects)}
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
