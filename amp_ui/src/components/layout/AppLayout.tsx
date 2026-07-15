// The authenticated app shell: fixed sidebar + sticky header, with the
// active route rendered in the scrollable main area. Wrap any
// authenticated route in this and it inherits the full chrome — pages
// only supply their centre-body content.

import { Outlet } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { Header } from "./Header";
import { AgentPanelProvider } from "./AgentPanel";
import { layoutStyles as s } from "./layoutStyles";
import { useVersion } from "@/lib/api/queries";

export function AppLayout() {
  return (
    <AgentPanelProvider>
      <div className={s.shell}>
        <Sidebar />
        <div className={s.content}>
          <Header />
          <main className={s.main}>
            <Outlet />
          </main>
          <AppFooter />
        </div>
      </div>
    </AgentPanelProvider>
  );
}

// Small persistent footer showing the running controller version (GET /version).
// The concise line shows amp vX.Y.Z · <commit>; the tooltip carries the full
// build identity, including the bundled storage_controller's version.
function AppFooter() {
  const { data } = useVersion();
  const commit =
    data?.git_commit && data.git_commit !== "unknown" ? data.git_commit : "";
  const title = data
    ? [
        `version ${data.version}`,
        commit ? `commit ${commit}` : "",
        data.build_date && data.build_date !== "unknown"
          ? `built ${data.build_date}`
          : "",
        data.storage_controller
          ? `storage_controller ${data.storage_controller}`
          : "",
      ]
        .filter(Boolean)
        .join(" · ")
    : undefined;
  return (
    <footer
      title={title}
      className="border-t px-4 py-1.5 text-center font-mono text-[11px] text-muted-foreground"
    >
      amp{data?.version ? ` v${data.version}` : ""}
      {commit ? ` · ${commit}` : ""}
    </footer>
  );
}
