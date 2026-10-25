// The left rail: the Meko mark, the Status link, the Extensions group, the datapacks, and the
// version (with a chip to the Status page's Updates card when a newer release is out), settings and
// theme toggle.

import { useState } from "react";
import { Database, Moon, Plus, Settings, Sun } from "lucide-react";
import { href, navigate, type Route } from "../lib/route";
import type { Theme } from "../lib/theme";
import { useUpdateAvailable } from "../lib/update";
import type { AuthStatus, Datapack } from "../lib/types";
import { DatapackDialog } from "./DatapackDialog";
import { ExtensionsLink } from "./ExtensionsLink";
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
  const update = useUpdateAvailable();
  const status = { tab: "overview" as const, view: "status" as const };
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
        <ExtensionsLink active={view === "extensions"} />
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
        <span className="flex min-w-0 items-center gap-1.5">
          <span className="truncate">Meko {version ?? ""}</span>
          {update && (
            <a
              href={href(status)}
              onClick={(e) => (e.preventDefault(), navigate(status))}
              title={`Meko ${update.latest} is available`}
              className="shrink-0 rounded bg-primary/15 px-1.5 py-0.5 text-[10px] font-semibold text-primary hover:bg-primary/25"
            >
              Update
            </a>
          )}
        </span>
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
