// The app shell: checks access, loads the datapacks, and shows the open datapack's tab beside the
// sidebar.

import { useCallback, useEffect, useState } from "react";
import { Database } from "lucide-react";
import { api, ApiError } from "./lib/api";
import { href, navigate, useRoute } from "./lib/route";
import { applyTheme, initialTheme, type Theme } from "./lib/theme";
import { clearToken } from "./lib/token";
import { useLoad } from "./lib/useLoad";
import { DatapackHeader } from "./components/DatapackHeader";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { Sidebar } from "./components/Sidebar";
import { Empty, ErrorNote, Loading } from "./components/ui";
import { ConnectPage } from "./pages/ConnectPage";
import { ConversationsPage } from "./pages/ConversationsPage";
import { KnowledgePage } from "./pages/KnowledgePage";
import { MemoriesPage } from "./pages/MemoriesPage";
import { NoAccessPage } from "./pages/NoAccessPage";
import { AnalyticsPage } from "./pages/AnalyticsPage";
import { ArtifactsPage } from "./pages/ArtifactsPage";
import { OverviewPage } from "./pages/OverviewPage";
import { SearchPage } from "./pages/SearchPage";
import { StatusPage } from "./pages/StatusPage";

export default function App() {
  const [theme, setTheme] = useState<Theme>(initialTheme);
  useEffect(() => applyTheme(theme), [theme]);

  const route = useRoute();
  const health = useLoad(() => api.health(), []);
  const list = useLoad(() => api.datapacks(), []);
  const activeId = route.datapackId ?? list.data?.[0]?.datapack_id;
  const current = useLoad(() => (activeId ? api.datapack(activeId) : Promise.resolve(undefined)), [activeId]);

  const reloadCurrent = current.reload;
  const onChanged = useCallback(() => reloadCurrent(), [reloadCurrent]);

  if (list.error instanceof ApiError && list.error.status === 401) {
    clearToken();
    return <NoAccessPage />;
  }

  return (
    <div className="flex h-screen overflow-hidden">
      <Sidebar
        datapacks={list.data ?? []}
        activeId={route.view ? undefined : activeId}
        statusOpen={route.view === "status"}
        version={health.data?.version}
        theme={theme}
        onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")}
        onCreated={(id) => {
          list.reload();
          navigate({ datapackId: id, tab: "overview" });
        }}
      />
      <main className="flex min-w-0 flex-1 flex-col overflow-y-auto">
        <ErrorBoundary key={href(route)}>
          {route.view === "status" ? (
            <div className="p-8">
              <StatusPage />
            </div>
          ) : list.loading && !list.data ? (
            <Loading />
          ) : list.error ? (
            <div className="p-8">
              <ErrorNote error={list.error} />
            </div>
          ) : !activeId ? (
            <Empty icon={<Database className="size-8" />} title="No datapacks">
              Create one with the + beside Datapacks.
            </Empty>
          ) : current.error ? (
            <div className="p-8">
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
              <div className="p-8">
                {route.tab === "overview" && <OverviewPage datapack={current.data} />}
                {route.tab === "analytics" && <AnalyticsPage datapack={current.data} onChanged={onChanged} />}
                {route.tab === "search" && <SearchPage datapack={current.data} />}
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
