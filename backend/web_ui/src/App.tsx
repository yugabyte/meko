// The app shell, once the gate (Gate.tsx) let this browser in: loads the datapacks, and shows the
// open datapack's tab, or the Status, Extensions, Meko Cloud or Account page, beside the sidebar (on
// a phone, a drawer opened from the top bar).

import { useCallback, useEffect, useState } from "react";
import { Database, Menu } from "lucide-react";
import { api } from "./lib/api";
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
import { ConversationsPage } from "./pages/ConversationsPage";
import { ExtensionsPage } from "./pages/ExtensionsPage";
import { KnowledgePage } from "./pages/KnowledgePage";
import { MekoCloudPage } from "./pages/MekoCloudPage";
import { MemoriesPage } from "./pages/MemoriesPage";
import { AccountPage } from "./pages/AccountPage";
import { MetricsPage } from "./pages/MetricsPage";
import { ArtifactsPage } from "./pages/ArtifactsPage";
import { OverviewPage } from "./pages/OverviewPage";
import { SearchPage } from "./pages/SearchPage";
import { SettingsPage } from "./pages/SettingsPage";
import { StatusPage } from "./pages/StatusPage";

// A page's padding: tighter on a phone.
const PAGE = "p-4 sm:p-8";

export default function App({ auth, onAuthChanged }: { auth: AuthStatus; onAuthChanged: () => void }) {
  const [theme, setTheme] = useState<Theme>(initialTheme);
  useEffect(() => applyTheme(theme), [theme]);

  const route = useRoute();
  const health = useLoad(() => api.health(), []);
  const list = useLoad(() => api.datapacks(), []);
  const activeId = route.datapackId ?? list.data?.[0]?.datapack_id;
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
        activeId={route.view ? undefined : activeId}
        view={route.view}
        version={health.data?.version}
        theme={theme}
        onToggleTheme={() => setTheme(theme === "dark" ? "light" : "dark")}
        onCreated={(id) => {
          list.reload();
          navigate({ datapackId: id, tab: "overview" });
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
          ) : route.view === "meko-cloud" ? (
            <div className={PAGE}>
              <MekoCloudPage />
            </div>
          ) : route.view === "account" ? (
            <div className={PAGE}>
              <AccountPage auth={auth} onChanged={onAuthChanged} />
            </div>
          ) : list.loading && !list.data ? (
            <Loading />
          ) : list.error ? (
            <div className={PAGE}>
              <ErrorNote error={list.error} />
            </div>
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
