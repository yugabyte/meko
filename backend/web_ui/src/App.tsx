// The app shell, once the gate (Gate.tsx) let this browser in: loads the datapacks, and shows the
// open datapack's tab, or the Status, Extensions, Settings or Meko Cloud page, beside the sidebar (on
// a phone, a drawer opened from the top bar).

import { useCallback, useEffect, useState } from "react";
import { Database, Menu } from "lucide-react";
import { api } from "./lib/api";
import { rowLink, useCloudChanged } from "./lib/cloud";
import { href, navigate, useNavigated, useRoute } from "./lib/route";
import { applyTheme, initialTheme, type Theme } from "./lib/theme";
import type { AuthStatus } from "./lib/types";
import { useLoad } from "./lib/useLoad";
import { useShortcuts } from "./lib/useShortcuts";
import { DatapackHeader } from "./components/DatapackHeader";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { Logo, Sidebar } from "./components/Sidebar";
import { Button, Empty, ErrorNote, Loading } from "./components/ui";
import { ConnectPage } from "./pages/ConnectPage";
import { CloudContextGapsPage, ContextGapsPage } from "./pages/ContextGapsPage";
import { ConversationsPage } from "./pages/ConversationsPage";
import { ExtensionsPage } from "./pages/ExtensionsPage";
import { KnowledgePage } from "./pages/KnowledgePage";
import { MekoCloudPage } from "./pages/MekoCloudPage";
import { MemoriesPage } from "./pages/MemoriesPage";
import { MetricsPage } from "./pages/MetricsPage";
import { ArtifactsPage } from "./pages/ArtifactsPage";
import { CallsPage } from "./pages/CallsPage";
import { OverviewPage } from "./pages/OverviewPage";
import { SearchPage } from "./pages/SearchPage";
import { ProjectPage } from "./pages/ProjectPage";
import { ProjectsPage } from "./pages/ProjectsPage";
import { SettingsPage } from "./pages/SettingsPage";
import { StatusPage } from "./pages/StatusPage";

// A page's padding: tighter on a phone.
const PAGE = "p-4 sm:p-8";

export default function App({ auth }: { auth: AuthStatus }) {
  const [theme, setTheme] = useState<Theme>(initialTheme);
  useEffect(() => applyTheme(theme), [theme]);

  const route = useRoute();
  const health = useLoad(() => api.health(), []);
  const list = useLoad(() => api.datapacks(), []);
  // Linking or unlinking Meko Cloud changes which datapacks there are.
  useCloudChanged(list.reload);
  // With none in the address, the first datapack this UI opens: a Meko Cloud one opens in its web app.
  const activeId = route.datapackId ?? list.data?.find((d) => rowLink(d).kind === "here")?.datapack_id;
  const current = useLoad(() => (activeId ? api.datapack(activeId) : Promise.resolve(undefined)), [activeId]);

  useShortcuts(activeId);

  // The phone drawer closes on every navigation, a tap on the page already open included.
  const [menu, setMenu] = useState(false);
  useNavigated(useCallback(() => setMenu(false), []));

  const reloadCurrent = current.reload;
  const onChanged = useCallback(() => reloadCurrent(), [reloadCurrent]);

  return (
    <div className="flex h-dvh overflow-hidden">
      <Sidebar
        open={menu}
        onClose={() => setMenu(false)}
        datapacks={list.data ?? []}
        loading={list.loading}
        error={list.error}
        onRefresh={list.reload}
        activeId={route.view ? undefined : activeId}
        view={route.view}
        version={health.data?.version}
        theme={theme}
        onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")}
        onCreated={(id, open) => {
          list.reload();
          if (open) navigate({ datapackId: id, tab: "overview" });
        }}
      />
      <main className="flex min-w-0 flex-1 flex-col overflow-y-auto">
        <div className="sticky top-0 z-30 flex items-center gap-2 border-b bg-sidebar px-2 py-2 md:hidden">
          <Button variant="ghost" onClick={() => setMenu(true)} aria-label="Open menu">
            <Menu className="size-5" />
          </Button>
          <Logo theme={theme} />
        </div>
        <ErrorBoundary key={href(route)}>
          {route.view === "status" ? (
            <div className={PAGE}>
              <StatusPage />
            </div>
          ) : route.view === "extensions" ? (
            <div className={PAGE}>
              <ExtensionsPage />
            </div>
          ) : route.view === "settings" ? (
            <div className={PAGE}>
              <SettingsPage auth={auth} />
            </div>
          ) : route.view === "calls" ? (
            <div className={PAGE}>
              <CallsPage />
            </div>
          ) : route.view === "projects" ? (
            <div className={PAGE}>
              <ProjectsPage />
            </div>
          ) : route.view === "project" && route.projectId ? (
            <div className={PAGE}>
              <ProjectPage id={route.projectId} />
            </div>
          ) : route.view === "meko-cloud" ? (
            <div className={PAGE}>
              <MekoCloudPage />
            </div>
          ) : route.view === "cloud-gaps" && route.datapackId ? (
            <div className={PAGE}>
              <CloudContextGapsPage id={route.datapackId} />
            </div>
          ) : list.loading && !list.data && !route.datapackId ? (
            // A datapack in the address doesn't wait for the listing, which waits for Meko Cloud while
            // it's linked; nor does a failed listing (Meko Cloud down) stop it from opening: the
            // sidebar shows the listing's error.
            <Loading />
          ) : list.error && !route.datapackId ? (
            <div className={PAGE}>
              <ErrorNote error={list.error} />
            </div>
          ) : !activeId && list.data?.length ? (
            // Every datapack listed opens in Meko Cloud's web app (or nowhere): none has pages here.
            <Empty icon={<Database className="size-8" />} title="Your datapacks are in Meko Cloud">
              Pick one under Datapacks to open it in Meko Cloud's web app. New ones made with the + go there too.
            </Empty>
          ) : !activeId ? (
            <Empty icon={<Database className="size-8" />} title="No datapacks">
              Create one with the + beside Datapacks.
            </Empty>
          ) : current.error ? (
            <div className={PAGE}>
              <ErrorNote error={current.error} />
            </div>
          ) : !current.data ? (
            <Loading />
          ) : (
            <>
              <DatapackHeader
                datapack={current.data}
                tab={route.tab}
                onEdited={() => {
                  list.reload();
                  current.reload();
                }}
                onDeleted={() => {
                  list.reload();
                  navigate({ tab: "overview" });
                }}
              />
              <div className={PAGE}>
                {route.tab === "overview" && <OverviewPage datapack={current.data} />}
                {route.tab === "metrics" && <MetricsPage datapack={current.data} onChanged={onChanged} />}
                {route.tab === "search" && <SearchPage datapack={current.data} />}
                {route.tab === "gaps" && <ContextGapsPage datapack={current.data} />}
                {route.tab === "memories" && <MemoriesPage datapack={current.data} routeScope={route.itemId} onChanged={onChanged} />}
                {route.tab === "knowledge" && <KnowledgePage datapack={current.data} onChanged={onChanged} />}
                {route.tab === "conversations" && (
                  <ConversationsPage datapack={current.data} conversationId={route.itemId} />
                )}
                {route.tab === "artifacts" && <ArtifactsPage datapack={current.data} />}
                {route.tab === "connect" && <ConnectPage datapack={current.data} />}
              </div>
            </>
          )}
        </ErrorBoundary>
      </main>
    </div>
  );
}
