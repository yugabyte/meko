// The authenticated app shell: fixed sidebar + sticky header, with the
// active route rendered in the scrollable main area. Wrap any
// authenticated route in this and it inherits the full chrome — pages
// only supply their centre-body content.

import { Outlet } from "react-router-dom";
import { Sidebar } from "./Sidebar";
import { Header } from "./Header";
import { AgentPanelProvider } from "./AgentPanel";
import { layoutStyles as s } from "./layoutStyles";

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
        </div>
      </div>
    </AgentPanelProvider>
  );
}
