// The left rail: the Meko mark, the Status link, AgentK's when it's installed, the datapacks, and
// the version, settings and theme toggle.

import { useState } from "react";
import { Bot, Database, Moon, Plus, Settings, Sun } from "lucide-react";
import { api } from "../lib/api";
import { href, navigate, type Route } from "../lib/route";
import type { Theme } from "../lib/theme";
import type { AuthStatus, Datapack } from "../lib/types";
import { useLoad } from "../lib/useLoad";
import { DatapackDialog } from "./DatapackDialog";
import { SettingsDialog } from "./SettingsDialog";
import { StatusLink } from "./StatusLink";
import { Button, cx } from "./ui";

export function Sidebar({
  datapacks,
  activeId,
  view,
  auth,
  version,
  theme,
  onToggleTheme,
  onCreated,
}: {
  datapacks: Datapack[];
  activeId?: string;
  view: Route["view"];
  auth: AuthStatus;
  version?: string;
  theme: Theme;
  onToggleTheme: () => void;
  onCreated: (id: string) => void;
}) {
  const [creating, setCreating] = useState(false);
  const [settings, setSettings] = useState(false);
  // An older server has no /extensions; the link just doesn't show.
  const extensions = useLoad(() => api.extensions(), []);
  const agentk = extensions.data?.find((e) => e.name === "agentk" && e.available);
  return (
    <aside className="flex w-60 shrink-0 flex-col border-r bg-sidebar">
      <div className="flex items-center gap-2 px-4 py-4">
        <img src={theme === "dark" ? "/meko-logo-white.svg" : "/meko-logo.svg"} alt="Meko" className="h-6" />
        <span className="rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-primary">
          Local
        </span>
      </div>
      <div className="px-2 pb-2">
        <StatusLink active={view === "status"} />
        {agentk && (
          // A plain link, opening AgentK in place of this page: AgentK refuses to be framed.
          <a
            href={agentk.path}
            className="flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm text-muted-foreground hover:bg-accent"
          >
            <Bot className="size-4 shrink-0" />
            AgentK
          </a>
        )}
      </div>

      <div className="flex items-center justify-between px-4 pb-1 pt-2">
        <span className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Datapacks</span>
        <Button size="sm" variant="ghost" onClick={() => setCreating(true)} title="New datapack">
          <Plus className="size-3.5" />
        </Button>
      </div>
      <nav className="flex-1 space-y-0.5 overflow-y-auto px-2">
        {datapacks.map((d) => (
          <a
            key={d.datapack_id}
            href={href({ datapackId: d.datapack_id, tab: "overview" })}
            onClick={(e) => {
              e.preventDefault();
              navigate({ datapackId: d.datapack_id, tab: "overview" });
            }}
            className={cx(
              "flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm",
              d.datapack_id === activeId ? "bg-accent font-medium" : "text-muted-foreground hover:bg-accent",
            )}
          >
            <Database className={cx("size-4 shrink-0", d.datapack_id === activeId && "text-primary")} />
            <span className="truncate">{d.datapack_name}</span>
          </a>
        ))}
      </nav>

      <div className="flex items-center justify-between border-t px-4 py-3 text-xs text-muted-foreground">
        <span>Meko {version ?? ""}</span>
        <div className="flex items-center">
          <Button size="sm" variant="ghost" onClick={() => setSettings(true)} title="Settings">
            <Settings className="size-3.5" />
          </Button>
          <Button size="sm" variant="ghost" onClick={onToggleTheme} title="Toggle theme">
            {theme === "dark" ? <Sun className="size-3.5" /> : <Moon className="size-3.5" />}
          </Button>
        </div>
      </div>

      {settings && <SettingsDialog auth={auth} onClose={() => setSettings(false)} />}

      {creating && (
        <DatapackDialog
          onClose={() => setCreating(false)}
          onSaved={(id) => {
            setCreating(false);
            onCreated(id);
          }}
        />
      )}
    </aside>
  );
}
